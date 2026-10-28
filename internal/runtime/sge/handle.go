package sge

import (
	"context"
	"sync"
	"time"

	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// jobHandle is the in-process view of one submitted job.
//
// Its instance is a private snapshot: on SGE the job's pid lives on a compute
// node, so PID() is 0 and the job id is the only identity this process holds.
type jobHandle struct {
	backend *Backend
	inst    *model.Instance
	script  string
	command []string

	stopOnce sync.Once
	stopErr  error
}

var _ runtime.Handle = (*jobHandle)(nil)

func (h *jobHandle) Ref() string { return h.inst.BackendRef }

// PID is 0 by design: a job script's pid on a compute node means nothing here.
func (h *jobHandle) PID() int { return 0 }

func (h *jobHandle) PIDStart() uint64 { return 0 }

func (h *jobHandle) Limiter() string { return "sge" }

func (h *jobHandle) Command() []string { return append([]string{}, h.command...) }

// Alive answers "is this a reachable instance?": the scheduler must still know
// the job and, when a tunnel is in use, the forward must still work.
func (h *jobHandle) Alive() bool {
	if t := h.backend.tunnelFor(h.inst.ID); t != nil && !t.Alive() {
		return false
	}
	obs, err := h.backend.Probe(context.Background(), h.inst)
	if err != nil {
		return false
	}
	return obs.Alive()
}

// Endpoint waits for readiness (bounded by budget for the placement phase) and
// returns the loopback port the gateway should dial. It returns
// runtime.ErrNotYetRunning when the job is submitted but not placed, which the
// manager turns into a `queued` record instead of a failed launch.
func (h *jobHandle) Endpoint(ctx context.Context, budget time.Duration) (*rendezvous.Ready, error) {
	host, port, err := h.backend.openEndpoint(ctx, h.inst, budget)
	if err != nil {
		return nil, err
	}
	return &rendezvous.Ready{
		Version:     1,
		Incarnation: h.inst.Incarnation,
		State:       "ready",
		Host:        host,
		Port:        port,
		Node:        h.inst.Node,
		Profile:     "",
		PublishedAt: h.backend.now().UTC(),
	}, nil
}

// DescribeEndpoint reports the compute node and job-side port openEndpoint
// resolved, so the control plane's record describes both ends of the tunnel.
func (h *jobHandle) DescribeEndpoint() (string, int) {
	return h.inst.Node, h.inst.JobPort
}

// Stop is qdel plus tunnel teardown, in that order: the compute slot is the
// costly resource. Idempotent, because Stop can race the reaper.
func (h *jobHandle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.backend.StopInstance(ctx, h.inst)
		h.backend.stopTunnel(ctx, h.inst.ID)
	})
	return h.stopErr
}

// LeaseExpiresAt is when SGE's h_rt will end the job, rounded to the whole
// second the scheduler itself sees.
func (h *jobHandle) LeaseExpiresAt(started time.Time) time.Time {
	d, err := parseWalltime(h.inst.Walltime)
	if err != nil || d <= 0 {
		return time.Time{}
	}
	return started.Add(d.Truncate(time.Second))
}
