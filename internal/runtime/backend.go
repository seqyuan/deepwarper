// Package runtime defines the seam between the control plane and the thing
// that actually runs a `dw instance` process.
//
// The local backend implements this with either a systemd user unit or a
// plain child process. Each user gets their own process on this machine.
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
)

// Phase is the normalised lifecycle phase of an instance as observed by its
// backend. It deliberately mirrors systemd's ActiveState vocabulary because
// systemd is the authority for unit mode, and maps the plain-process case
// onto the subset that makes sense there.
type Phase string

const (
	PhaseUnknown      Phase = "unknown"
	PhaseActive       Phase = "active"
	PhaseActivating   Phase = "activating"
	PhaseReloading    Phase = "reloading"
	PhaseDeactivating Phase = "deactivating"
	PhaseFailed       Phase = "failed"
	PhaseInactive     Phase = "inactive"
	PhaseNotFound     Phase = "not-found"
)

// Alive reports whether the backend still owns a process we must not orphan.
// activating/deactivating/reloading are alive on purpose: systemd is still
// working on the unit, and releasing a port or deleting a record mid-restart
// would leak a live instance.
func (p Phase) Alive() bool {
	switch p {
	case PhaseActive, PhaseActivating, PhaseReloading, PhaseDeactivating:
		return true
	default:
		return false
	}
}

// Gone reports a confirmed death. unknown is never gone: a transient backend
// hiccup must not orphan a healthy instance.
func (p Phase) Gone() bool {
	switch p {
	case PhaseFailed, PhaseInactive, PhaseNotFound:
		return true
	default:
		return false
	}
}

// Observation is the backend's answer to "what is this instance doing right
// now?". It is intentionally a value, not an error, for the dead/alive
// decision so callers cannot accidentally treat a probe failure as death.
type Observation struct {
	Phase  Phase  `json:"phase"`
	Source string `json:"source,omitempty"` // systemd | pid
	Ref    string `json:"ref,omitempty"`    // unit name when applicable
	PID    int    `json:"pid,omitempty"`
	// PIDStart is the /proc/<pid>/stat starttime used to reject reused pids.
	PIDStart uint64 `json:"pid_start,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	// Detail carries systemd's Result= and/or the launcher verdict.
	Detail string `json:"detail,omitempty"`
	// Verdict is the content of <log>.verdict written by ExecStopPost.
	Verdict string `json:"verdict,omitempty"`
	// NegativeReadings counts consecutive "looks dead" probes. Unit mode
	// only reports Gone after three of them.
	NegativeReadings int `json:"negative_readings,omitempty"`
	// Note is human text for the record or the UI. It must never carry a
	// credential: it is copied into model.Instance.Error and status JSON.
	Note string `json:"note,omitempty"`
}

func (o Observation) Alive() bool { return o.Phase.Alive() }
func (o Observation) Gone() bool  { return o.Phase.Gone() }

// Describe renders a one-line explanation suitable for model.Instance.Error.
func (o Observation) Describe() string {
	var b strings.Builder
	if o.Ref != "" {
		fmt.Fprintf(&b, "unit %s: %s", o.Ref, o.Phase)
	} else {
		fmt.Fprintf(&b, "process %d: %s", o.PID, o.Phase)
	}
	var bits []string
	if o.Note != "" {
		bits = append(bits, o.Note)
	}
	if o.Detail != "" {
		bits = append(bits, o.Detail)
	}
	if o.Verdict != "" {
		bits = append(bits, fmt.Sprintf("verdict=%q", o.Verdict))
	}
	if o.ExitCode != 0 {
		bits = append(bits, fmt.Sprintf("exit=%d", o.ExitCode))
	}
	if len(bits) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(bits, "; "))
		b.WriteString(")")
	}
	return b.String()
}

// Handle is one started instance. A backend may stop it through the handle
// while this process is alive, or through Backend.Stop(inst) after a control
// plane restart.
type Handle interface {
	// Ref is the backend's own handle (a unit name for unit mode). It may be
	// empty for plain process mode, where PID/PIDStart are authoritative.
	Ref() string
	PID() int
	PIDStart() uint64
	Limiter() string
	Command() []string
	Alive() bool
	Endpoint(ctx context.Context, timeout time.Duration) (*rendezvous.Ready, error)
	Stop(ctx context.Context) error
}

// Backend starts, stops and observes instances.
//
// Start must not wait for dsh readiness; callers use Handle.Endpoint for that.
// Stop must be idempotent and must work for records restored from disk, when
// no in-memory Handle exists.
type Backend interface {
	Name() string
	Start(ctx context.Context, spec agent.Spec) (Handle, error)
	Stop(ctx context.Context, inst *model.Instance) error
	Probe(ctx context.Context, inst *model.Instance) (Observation, error)
}
