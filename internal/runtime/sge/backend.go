package sge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// goneConfirmations is how many consecutive "the scheduler does not know this
// job" readings are required before a job is believed gone.
//
// One is not enough: qstat is silent for both "job unknown" and "scheduler or
// shared-filesystem hiccup", so a single bad reading must never tear down a live
// route. An unknown phase (a failed qstat call) is not evidence and resets the
// counter, which is stricter than counting it as a miss on purpose: a setting
// that only produces false negatives is worse than a slower settle.
const goneConfirmations = 3

// Backend implements runtime.Backend for SGE. The struct itself lives in
// config.go next to its configuration; this file is the job lifecycle.

// Start writes the spec, the job script and submits. Submission is synchronous:
// qsub returns a job id or fails, and a failure here means the unit never existed
// (a control-plane error), whereas a job that is submitted and then dies reports
// through its own state/exit_code files.
func (b *Backend) Start(ctx context.Context, spec agent.Spec) (runtime.Handle, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.ControlDir) == "" {
		return nil, errors.New("sge backend: spec.ControlDir is required")
	}
	if strings.TrimSpace(spec.UnitName) == "" {
		return nil, errors.New("sge backend: spec.UnitName is required")
	}
	if strings.TrimSpace(spec.Incarnation) == "" {
		return nil, errors.New("sge backend: spec.Incarnation is required")
	}
	// The job log must live inside the bind set (the control directory), because
	// dataDir as a whole is not mounted into the container.
	host := spec
	host.LogPath = filepath.Join(host.ControlDir, "job.log")

	if err := os.MkdirAll(b.SubmitDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(host.ControlDir, 0o700); err != nil {
		return nil, err
	}
	// A stale ready.json from a previous incarnation would make a readiness check
	// pass against a dead process.
	if err := rendezvous.Clear(host.ControlDir); err != nil {
		return nil, err
	}

	box := b.boxSpec(host)
	data, err := json.MarshalIndent(box, "", "  ")
	if err != nil {
		return nil, err
	}
	// spec.json is written before qsub so a failed submit still leaves something
	// diagnosable on disk.
	if err := rendezvous.Write(host.ControlDir, "spec.json", data, 0o600); err != nil {
		return nil, err
	}

	script, err := b.renderScript(host)
	if err != nil {
		return nil, err
	}
	scriptPath := filepath.Join(b.SubmitDir, scriptName(host.UnitName))
	if err := writeFileAtomic(scriptPath, []byte(script), 0o755); err != nil {
		return nil, err
	}

	args, err := b.headerArgs(host)
	if err != nil {
		return nil, err
	}
	args = append(args, scriptPath)
	out, err := b.runner().Run(ctx, b.bin("qsub"), args...)
	if err != nil {
		return nil, fmt.Errorf("qsub %s: %w: %s", scriptPath, err, firstLine(out))
	}
	jobID, err := parseQsubOutput(out)
	if err != nil {
		return nil, fmt.Errorf("qsub said %q: %w", firstLine(out), err)
	}
	// The login node records the id under the same name the job script uses, so a
	// reader has one field to look at no matter which side wrote it last.
	if err := rendezvous.WriteString(host.ControlDir, "jobid", jobID, 0o644); err != nil {
		return nil, err
	}
	// state=submitted only if the job has not already moved on: a job that starts
	// (and maybe fails) before qsub returns must not have its state overwritten
	// with an older one.
	if _, err := rendezvous.Read(host.ControlDir, "state"); err != nil {
		if err := rendezvous.WriteString(host.ControlDir, "state", "submitted", 0o644); err != nil {
			return nil, err
		}
	}

	now := b.now()
	inst := &model.Instance{
		ID:          host.InstanceID,
		BackendRef:  jobID,
		Backend:     "sge",
		Incarnation: host.Incarnation,
		UnitName:    host.UnitName,
		ControlDir:  host.ControlDir,
		LogPath:     host.LogPath,
		Port:        host.Port, // the login-node loopback port the gateway dials
		StartedAt:   now,
		CPU:         host.CPU,
		Memory:      host.Memory,
		Walltime:    host.Walltime,
		GPU:         host.GPU,
		State:       model.StateStarting,
		CreatedAt:   now,
	}
	h := &jobHandle{
		backend: b,
		inst:    inst,
		script:  scriptPath,
		command: append([]string{b.bin("qsub")}, args...),
	}
	return h, nil
}

// Probe asks the scheduler what the job is doing.
//
// `qstat -xml` is invoked bare (never `qstat -xml -j <id>`): the -j form emits a
// <detailed_job_info> document with no <job_list>/<state>, so every liveness
// query would answer "unknown". Bare qstat is scoped to the invoking user, which
// is exactly the set this control plane owns.
func (b *Backend) Probe(ctx context.Context, inst *model.Instance) (runtime.Observation, error) {
	if inst == nil {
		return runtime.Observation{Phase: runtime.PhaseUnknown}, nil
	}
	ref := strings.TrimSpace(inst.BackendRef)
	if ref == "" {
		return runtime.Observation{Phase: runtime.PhaseUnknown, Source: "qstat", Note: "no job id recorded"}, nil
	}
	jobs, err := b.qstat(ctx)
	if err != nil {
		// A qstat failure says nothing about the job: never report it as gone,
		// and reset the confirmation count because an outage is not evidence.
		b.clearMiss(inst.ID)
		return runtime.Observation{Phase: runtime.PhaseUnknown, Source: "qstat", Ref: ref, Note: firstLine(err.Error())}, err
	}
	j, ok := FindJob(jobs, ref)
	if !ok {
		n := b.bumpMiss(inst.ID)
		obs := runtime.Observation{
			Phase:  runtime.PhaseGone,
			Source: "qstat",
			Ref:    ref,
			Node:   inst.Node,
			Note:   fmt.Sprintf("job %s is not in qstat", ref),
		}
		if n < goneConfirmations {
			obs.Phase = runtime.PhaseUnknown
			obs.NegativeReadings = n
			obs.Note = fmt.Sprintf("pending gone confirmation (%d/%d): job %s is not in qstat", n, goneConfirmations, ref)
		}
		return obs, nil
	}
	b.clearMiss(inst.ID)

	obs := runtime.Observation{Phase: j.State.Phase(), Source: "qstat", Ref: j.ID, Node: j.Node}
	switch obs.Phase {
	case runtime.PhaseQueued:
		if pos, ok := QueuePosition(jobs, j.ID); ok {
			obs.QueuePos = pos
			if j.Queue != "" {
				obs.Note = fmt.Sprintf("queued: position %d in %s", pos, j.Queue)
			} else {
				obs.Note = fmt.Sprintf("queued: position %d", pos)
			}
		}
	case runtime.PhaseSuspended:
		obs.Note = "job is suspended by the scheduler"
	case runtime.PhaseFailed:
		obs.Note = fmt.Sprintf("job entered state %s", j.State.Raw)
	}
	return obs, nil
}

// qstat returns the parsed job list, cached for one poll interval so a tick with
// many instances still costs one scheduler call.
func (b *Backend) qstat(ctx context.Context) ([]Job, error) {
	b.mu.Lock()
	if !b.jobsAt.IsZero() && b.now().Sub(b.jobsAt) < b.PollEvery {
		jobs, err := b.jobs, b.jobsErr
		b.mu.Unlock()
		return jobs, err
	}
	b.mu.Unlock()

	tick := b.now()
	out, err := b.runner().Run(ctx, b.bin("qstat"), "-xml")
	var jobs []Job
	var qerr error
	if err != nil {
		qerr = fmt.Errorf("qstat -xml: %w: %s", err, firstLine(out))
	} else {
		jobs, qerr = ParseQstatXML(out)
		if qerr != nil {
			qerr = fmt.Errorf("qstat -xml: %w", qerr)
		}
	}
	b.mu.Lock()
	b.jobs, b.jobsAt, b.jobsErr = jobs, tick, qerr
	b.mu.Unlock()
	return jobs, qerr
}

// InvalidateCache drops the cached job list. Tests use it to script one qstat
// answer per probe without depending on clock resolution.
func (b *Backend) InvalidateCache() {
	b.mu.Lock()
	b.jobsAt = time.Time{}
	b.mu.Unlock()
}

func (b *Backend) bumpMiss(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.misses == nil {
		b.misses = map[string]int{}
	}
	b.misses[id]++
	return b.misses[id]
}

func (b *Backend) clearMiss(id string) {
	b.mu.Lock()
	delete(b.misses, id)
	b.mu.Unlock()
}

// StopInstance deletes the job from the persisted record only, which is what
// makes it work after a control-plane restart. Idempotent: "job not found" is
// success, because a race with completion is the normal case.
func (b *Backend) StopInstance(ctx context.Context, inst *model.Instance) error {
	if inst == nil {
		return nil
	}
	ref := strings.TrimSpace(inst.BackendRef)
	if ref == "" {
		return nil // never submitted
	}
	out, err := b.runner().Run(ctx, b.bin("qdel"), ref)
	if err != nil {
		msg := strings.ToLower(out)
		if strings.Contains(msg, "not found") || strings.Contains(msg, "unknown job") || strings.Contains(msg, "没有") {
			return nil
		}
		return fmt.Errorf("qdel %s: %w: %s", ref, err, firstLine(out))
	}
	_ = rendezvous.WriteString(inst.ControlDir, "state", "deleted", 0o644)
	return nil
}

// Stop implements runtime.Backend: qdel first (the compute slot is the costly
// resource), then tear the data channel down.
func (b *Backend) Stop(ctx context.Context, inst *model.Instance) error {
	err := b.StopInstance(ctx, inst)
	if inst != nil {
		b.stopTunnel(ctx, inst.ID)
	}
	return err
}

// ── tunnel registry ───────────────────────────────────────────────────────

func (b *Backend) tunnelFor(id string) TunnelRuntime {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tunnels[id]
}

func (b *Backend) setTunnel(id string, t TunnelRuntime) {
	b.mu.Lock()
	if b.tunnels == nil {
		b.tunnels = map[string]TunnelRuntime{}
	}
	old := b.tunnels[id]
	b.tunnels[id] = t
	b.mu.Unlock()
	if old != nil && old != t {
		_ = old.Stop(context.Background())
	}
}

func (b *Backend) stopTunnel(ctx context.Context, id string) {
	b.mu.Lock()
	t := b.tunnels[id]
	delete(b.tunnels, id)
	b.mu.Unlock()
	if t != nil {
		_ = t.Stop(ctx)
	}
}

// ── small helpers ─────────────────────────────────────────────────────────

// scriptName is the job script's file name. It carries no incarnation suffix, so
// a site's submitDir does not grow one file per restart.
func scriptName(unitName string) string {
	name := strings.TrimSuffix(unitName, ".service")
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, name)
	if name == "" {
		name = "dw-job"
	}
	return name + ".sh"
}

// writeFileAtomic writes a file so a concurrent reader never sees it half
// written (the compute node may start before qsub returns).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Policy methods (runtime.Policy).
//
// LaunchBudget is zero on SGE by default: a job can sit in a queue for hours, so
// Ensure returns a `queued` record instead of blocking a platform request.
// IdleReapAllowed is false because the lifecycle ceiling here is h_rt, and
// killing a queued or preempted job to force a re-queue is user-hostile.
func (b *Backend) LaunchBudget() time.Duration { return b.LaunchWait }
func (b *Backend) PollInterval() time.Duration { return b.PollEvery }
func (b *Backend) IdleReapAllowed() bool       { return b.IdleReap }

var (
	_ runtime.Backend     = (*Backend)(nil)
	_ runtime.Policy      = (*Backend)(nil)
	_ runtime.Adopter     = (*Backend)(nil)
	_ runtime.Renewer     = (*Backend)(nil)
	_ runtime.LeaseWarner = (*Backend)(nil)
)
