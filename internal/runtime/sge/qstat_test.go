package sge

import (
	"testing"

	"github.com/seqyuan/deepwarper/internal/runtime"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		raw       string
		phase     runtime.Phase
		alive     bool
		pending   bool
		suspended bool
		failed    bool
	}{
		{raw: "qw", phase: runtime.PhaseQueued, alive: true, pending: true},
		{raw: "h", phase: runtime.PhaseQueued, alive: true, pending: true},
		{raw: "t", phase: runtime.PhaseQueued, alive: true, pending: true},
		{raw: "T", phase: runtime.PhaseQueued, alive: true, pending: true},
		{raw: "r", phase: runtime.PhaseRunning, alive: true},
		{raw: "R", phase: runtime.PhaseRunning, alive: true},
		// Suspended is alive on purpose: SGE preempts with SIGSTOP and may resume.
		{raw: "s", phase: runtime.PhaseSuspended, alive: true, suspended: true},
		{raw: "S", phase: runtime.PhaseSuspended, alive: true, suspended: true},
		{raw: "d", phase: runtime.PhaseGone},
		{raw: "Eqw", phase: runtime.PhaseFailed, failed: true},
		// An unknown letter stays alive: guessing "dead" destroys work.
		{raw: "Z", phase: runtime.PhaseQueued, alive: true, pending: true},
		{raw: "", phase: runtime.PhaseUnknown},
	}
	for _, c := range cases {
		st := classify(c.raw)
		if got := st.Phase(); got != c.phase {
			t.Fatalf("classify(%q).Phase() = %s want %s", c.raw, got, c.phase)
		}
		if st.Alive() != c.alive {
			t.Fatalf("classify(%q).Alive() = %v want %v", c.raw, st.Alive(), c.alive)
		}
		if st.Pending != c.pending || st.Suspended != c.suspended || st.Failed != c.failed {
			t.Fatalf("classify(%q) flags = %+v", c.raw, st)
		}
	}
}

func TestParseMixedDocument(t *testing.T) {
	jobs, err := ParseQstatXML(readFixture(t, "qstat-mixed.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 5 {
		t.Fatalf("jobs: %d (%+v)", len(jobs), jobs)
	}
	byID := map[string]Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if j := byID["1001"]; j.State.Phase() != runtime.PhaseRunning || j.Queue != "batch.q@node001" || j.Node != "node001" || j.Slots != 4 {
		t.Fatalf("1001: %+v", j)
	}
	if j := byID["1002"]; j.State.Phase() != runtime.PhaseQueued || j.Slots != 2 {
		t.Fatalf("1002: %+v", j)
	}
	if j := byID["1003"]; j.State.Phase() != runtime.PhaseSuspended {
		t.Fatalf("1003: %+v", j)
	}
	if j := byID["1004"]; j.State.Phase() != runtime.PhaseQueued {
		t.Fatalf("1004 (hold is alive): %+v", j)
	}
	if j := byID["1005"]; j.State.Phase() != runtime.PhaseFailed {
		t.Fatalf("1005: %+v", j)
	}
}

func TestParseStateElementOverridesAttribute(t *testing.T) {
	doc := `<queue_info>
  <job_list state="running">
    <JB_job_number>7</JB_job_number>
    <state>Eqw</state>
  </job_list>
</queue_info>`
	jobs, err := ParseQstatXML(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State.Phase() != runtime.PhaseFailed {
		t.Fatalf("element must win over attribute: %+v", jobs)
	}
}

func TestParseTruncatedReturnsPartial(t *testing.T) {
	jobs, err := ParseQstatXML(readFixture(t, "qstat-truncated.xml"))
	if err != nil {
		t.Fatalf("a truncated document must yield what it has: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "2001" {
		t.Fatalf("jobs: %+v", jobs)
	}
}

// TestParseDetailedJobInfoYieldsNothing is the `qstat -xml -j` regression: that
// document has no job_list/state, so a parser that read it would answer
// "unknown" for every liveness query.
func TestParseDetailedJobInfoYieldsNothing(t *testing.T) {
	jobs, err := ParseQstatXML(readFixture(t, "qstat-detailed-job-info.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("detailed_job_info must yield no jobs: %+v", jobs)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	jobs, err := ParseQstatXML(readFixture(t, "qstat-empty.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs: %+v", jobs)
	}
}

func TestQueuePosition(t *testing.T) {
	jobs, err := ParseQstatXML(readFixture(t, "qstat-mixed.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if pos, ok := QueuePosition(jobs, "1002"); !ok || pos != 1 {
		t.Fatalf("1002 position: %d/%v", pos, ok)
	}
	if pos, ok := QueuePosition(jobs, "1004"); !ok || pos != 2 {
		t.Fatalf("1004 position: %d/%v", pos, ok)
	}
	if _, ok := QueuePosition(jobs, "1001"); ok {
		t.Fatal("a running job has no queue position")
	}
}

func TestFindJobIgnoresTaskSuffix(t *testing.T) {
	jobs, err := ParseQstatXML(readFixture(t, "qstat-mixed.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if j, ok := FindJob(jobs, "1001.1"); !ok || j.ID != "1001" {
		t.Fatalf("task suffix: %+v/%v", j, ok)
	}
	jobs = append(jobs, Job{ID: "5555.3", State: classify("r")})
	if j, ok := FindJob(jobs, "5555"); !ok || j.ID != "5555.3" {
		t.Fatalf("base id: %+v/%v", j, ok)
	}
}
