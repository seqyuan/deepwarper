package sge

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/seqyuan/deepwarper/internal/runtime"
)

// JobState is SGE's view of a job, normalised.
//
// The distinction that matters most is Suspended versus Gone: SGE preempts with
// SIGSTOP and may requeue, so a suspended job is still the user's work and must
// not be torn down as if it had died.
type JobState struct {
	// Raw is the state string as reported ("qw", "r", "s", "Eqw", ...).
	Raw string
	// Unknown means the scheduler does not know this job: it finished, was
	// deleted, or never existed.
	Unknown bool
	// Pending: queued, waiting for a slot.
	Pending bool
	// Running: has a slot.
	Running bool
	// Suspended: preempted, held or manually suspended. Alive, not progressing.
	Suspended bool
	// Failed: a state that needs a decision (Eqw and friends).
	Failed bool
}

// Alive reports whether the job still exists in the scheduler.
//
// Suspended counts as alive: it holds its slot and SGE may resume it. Treating
// it as dead is the mistake that makes a platform resubmit paused work.
func (s JobState) Alive() bool {
	return !s.Unknown && (s.Pending || s.Running || s.Suspended)
}

// Waiting reports whether the job has not started yet.
func (s JobState) Waiting() bool { return s.Pending }

// Phase maps the scheduler's vocabulary onto the runtime's.
func (s JobState) Phase() runtime.Phase {
	switch {
	case strings.TrimSpace(s.Raw) == "":
		// A job_list with no state element is a partial document, not a death
		// certificate.
		return runtime.PhaseUnknown
	case s.Failed:
		return runtime.PhaseFailed
	case s.Suspended:
		return runtime.PhaseSuspended
	case s.Running:
		return runtime.PhaseRunning
	case s.Pending:
		return runtime.PhaseQueued
	default:
		return runtime.PhaseGone
	}
}

func (s JobState) String() string {
	switch {
	case s.Unknown:
		return "unknown"
	case s.Failed:
		return "failed(" + s.Raw + ")"
	case s.Suspended:
		return "suspended"
	case s.Running:
		return "running"
	case s.Pending:
		return "pending"
	}
	return s.Raw
}

// classify maps an SGE state string onto JobState.
//
// SGE states seen in `qstat -xml`:
//
//	qw  queued, waiting            r   running
//	s   suspended (also `S`)       h   hold (user or system)
//	R   restarted                  d   deleted
//	E   error                      Eqw error, queued and waiting
//	t   transferring               T   threshold
//
// Unrecognised letters keep the job alive and pending rather than being mapped
// to a terminal state: guessing "dead" destroys work that is actually in flight.
func classify(raw string) JobState {
	st := JobState{Raw: raw}
	body := strings.TrimSpace(raw)
	if body == "" {
		st.Unknown = true
		return st
	}
	if strings.HasPrefix(body, "E") {
		st.Failed = true
		return st
	}
	for _, c := range body {
		switch c {
		case 'q', 'w', 'h', 't', 'T':
			st.Pending = true
		case 'r', 'R':
			st.Running = true
		case 's', 'S':
			st.Suspended = true
		case 'd':
			// Deleted is on its way out: treat it as gone so the caller stops
			// waiting, and let the job's own exit_code be the reason.
			st.Unknown = true
			return st
		default:
			st.Pending = true
		}
	}
	return st
}

// qstatXML mirrors the subset of SGE's XML this backend reads.
//
// SGE emits two containers: queue_info for jobs that hold a slot, job_info for
// jobs that do not (queued, held, or finished but still listed). Both are read,
// because a queued service is still a service.
type qstatXML struct {
	QueueInfo struct {
		Jobs []xmlJobList `xml:"job_list"`
	} `xml:"queue_info"`
	JobInfo struct {
		Jobs []xmlJobList `xml:"job_list"`
	} `xml:"job_info"`
}

type xmlJobList struct {
	State    string `xml:"state,attr"`
	JobNum   string `xml:"JB_job_number"`
	JobName  string `xml:"JB_name"`
	Owner    string `xml:"JB_owner"`
	StateEl  string `xml:"state"`
	TaskID   string `xml:"JB_taskid"`
	QueueRaw string `xml:"queue_name"`
	Slots    string `xml:"slots"`
}

// Job is one parsed job.
type Job struct {
	ID     string
	Name   string
	Owner  string
	Queue  string
	Node   string
	Slots  int
	State  JobState
	TaskID string
}

// ParseQstatXML parses `qstat -xml`.
//
// It is deliberately tolerant: a scheduler that emits a partial document (which
// happens while jobs are transitioning) yields what it can rather than an error,
// because the caller's question is "is this alive?" and a parse failure would
// answer it with a wrong "no".
func ParseQstatXML(data string) ([]Job, error) {
	dec := xml.NewDecoder(strings.NewReader(data))
	dec.Strict = false

	var jobs []Job
	var cur *xmlJobList
	var stack []string

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(jobs) > 0 {
				return jobs, nil // partial document, but something usable
			}
			return nil, fmt.Errorf("parse qstat -xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if t.Name.Local == "job_list" {
				cur = &xmlJobList{}
				for _, a := range t.Attr {
					if a.Name.Local == "state" {
						cur.State = a.Value
					}
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if t.Name.Local == "job_list" && cur != nil {
				if j, ok := cur.toJob(); ok {
					jobs = append(jobs, j)
				}
				cur = nil
			}
		case xml.CharData:
			if cur == nil || len(stack) == 0 {
				continue
			}
			// Only leaf elements carry scalars; the container names would
			// otherwise overwrite them with whitespace.
			switch stack[len(stack)-1] {
			case "JB_job_number":
				cur.JobNum = strings.TrimSpace(string(t))
			case "JB_name":
				cur.JobName = strings.TrimSpace(string(t))
			case "JB_owner":
				cur.Owner = strings.TrimSpace(string(t))
			case "state":
				cur.StateEl = strings.TrimSpace(string(t))
			case "queue_name":
				cur.QueueRaw = strings.TrimSpace(string(t))
			case "slots":
				cur.Slots = strings.TrimSpace(string(t))
			case "JB_taskid":
				cur.TaskID = strings.TrimSpace(string(t))
			}
		}
	}
	return jobs, nil
}

func (x *xmlJobList) toJob() (Job, bool) {
	if x.JobNum == "" {
		return Job{}, false // a job_list without an id is not a job
	}
	raw := x.StateEl
	if raw == "" {
		raw = x.State
	}
	j := Job{
		ID:     x.JobNum,
		Name:   x.JobName,
		Owner:  x.Owner,
		Queue:  x.QueueRaw,
		Node:   queueNode(x.QueueRaw),
		TaskID: x.TaskID,
		State:  classify(raw),
	}
	if n, err := parseLeadingInt(x.Slots); err == nil {
		j.Slots = n
	}
	return j, true
}

// queueNode extracts the node from "batch.q@node001".
func queueNode(queue string) string {
	if i := strings.LastIndexByte(queue, '@'); i >= 0 {
		return strings.TrimSpace(queue[i+1:])
	}
	return ""
}

func parseLeadingInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	n, digits := 0, 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("no leading integer in %q", s)
	}
	return n, nil
}

// baseJobID strips an array-task suffix: "12345.1" -> "12345".
func baseJobID(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return id
}

// FindJob returns the job with the given id, ignoring a task suffix on either
// side: a record that says 12345 finds 12345.1, and vice versa.
func FindJob(jobs []Job, id string) (Job, bool) {
	want := baseJobID(strings.TrimSpace(id))
	if want == "" {
		return Job{}, false
	}
	for _, j := range jobs {
		if baseJobID(j.ID) == want {
			return j, true
		}
	}
	return Job{}, false
}

// QueuePosition returns the job's 1-based position among pending jobs, which is
// what a user waiting for a slot actually wants to see.
func QueuePosition(jobs []Job, id string) (int, bool) {
	want := baseJobID(strings.TrimSpace(id))
	pos := 0
	for _, j := range jobs {
		if !j.State.Pending {
			continue
		}
		pos++
		if baseJobID(j.ID) == want {
			return pos, true
		}
	}
	return 0, false
}
