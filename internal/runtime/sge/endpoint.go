package sge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// awaitReady is two-phase readiness: queue is not slow start.
//
// Phase A (placement) polls the scheduler. A job can legitimately wait hours for
// a slot, so the readiness budget does not start until qstat first reports `r`.
// Phase B (readiness) waits for the job to publish ready.json.
func (b *Backend) awaitReady(ctx context.Context, inst *model.Instance, budget time.Duration) (*rendezvous.Ready, error) {
	start := b.now()
	launchDeadline := start.Add(budget)
	var queueDeadline time.Time
	if b.QueueTimeout > 0 {
		queueDeadline = start.Add(b.QueueTimeout)
	}
	var readyDeadline time.Time
	running := false

	for {
		// The job's own terminal record is the strongest signal: it settles the
		// question immediately, without waiting for the scheduler's view.
		if state, err := rendezvous.Read(inst.ControlDir, "state"); err == nil && terminalState(state) {
			code, _ := rendezvous.Read(inst.ControlDir, "exit_code")
			return nil, fmt.Errorf("instance exited before ready (state=%s exit=%s)", state, code)
		}

		obs, probeErr := b.Probe(ctx, inst)
		if probeErr == nil {
			switch obs.Phase {
			case runtime.PhaseFailed:
				return nil, fmt.Errorf("SGE job %s: %s", inst.BackendRef, strings.TrimSpace(obs.Note))
			case runtime.PhaseGone:
				return nil, fmt.Errorf("SGE job %s vanished before it started", inst.BackendRef)
			case runtime.PhaseRunning, runtime.PhaseActive:
				if !running {
					running = true
					readyDeadline = b.now().Add(b.ReadyWait)
				}
			case runtime.PhaseQueued, runtime.PhaseSuspended:
				// Still waiting for a slot, or paused by the scheduler. Neither is
				// a failure.
			}
		}

		// A ready.json published by this incarnation is always authoritative,
		// whether the scheduler already calls the job running or not.
		if r, err := rendezvous.ReadReady(inst.ControlDir); err == nil &&
			r.Incarnation == inst.Incarnation && r.State == "ready" && r.Port > 0 {
			return r, nil
		}

		// queueTimeout is an explicit operator choice, so enforce it even on the
		// zero-budget path. A job that never gets a slot must not hold a pooled
		// port forever.
		if !running && !queueDeadline.IsZero() && b.now().After(queueDeadline) {
			err := fmt.Errorf("job %s did not start within %s (still queued)", inst.BackendRef, b.QueueTimeout)
			_ = b.StopInstance(ctx, inst)
			return nil, err
		}

		// launchWait=0 means "probe once, never block": Ensure must return a
		// queued record immediately and the fast tick will promote it. This also
		// keeps a reconcile tick from sleeping up to readyTimeout after qstat has
		// just started reporting `r`.
		if budget <= 0 {
			return nil, runtime.ErrNotYetRunning
		}

		if running {
			if !readyDeadline.IsZero() && b.now().After(readyDeadline) {
				return nil, fmt.Errorf("job %s: timed out after %s waiting for ready.json", inst.BackendRef, b.ReadyWait)
			}
		} else {
			if b.now().After(launchDeadline) {
				return nil, runtime.ErrNotYetRunning
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.pollSleep()):
		}
	}
}

// pollSleep keeps the readiness loop from spinning when pollEvery is tiny in a
// test while staying responsive on a cluster.
func (b *Backend) pollSleep() time.Duration {
	d := b.PollEvery
	if d < 5*time.Millisecond {
		d = 5 * time.Millisecond
	}
	return d
}

// nodeFor picks the compute node the tunnel must target.
//
// Precedence: the file the *job script* wrote on the compute node (`hostname` is
// ground truth there), then ready.json's node written inside the container
// (`--containall` implies a UTS namespace, so it is only a fallback). Either way
// the value is validated before it reaches ssh.
func (b *Backend) nodeFor(inst *model.Instance, r *rendezvous.Ready) (string, error) {
	if raw, err := rendezvous.Read(inst.ControlDir, "node"); err == nil {
		node := strings.TrimSpace(raw)
		if ValidHost(node) {
			return node, nil
		}
		return "", fmt.Errorf("job %s published an unusable node name %q", inst.BackendRef, node)
	}
	if r != nil && ValidHost(r.Node) {
		return r.Node, nil
	}
	return "", fmt.Errorf("job %s published no usable node name, so it cannot be tunnelled to", inst.BackendRef)
}

// openEndpoint is the single implementation of "wait until ready, then make it
// reachable". Both the in-process handle and Adopt call it, which is what makes
// "a restart behaves like a first start" true.
func (b *Backend) openEndpoint(ctx context.Context, inst *model.Instance, budget time.Duration) (string, int, error) {
	r, err := b.awaitReady(ctx, inst, budget)
	if err != nil {
		return "", 0, err
	}
	node, err := b.nodeFor(inst, r)
	if err != nil {
		return "", 0, err
	}
	inst.Node = node
	inst.JobPort = r.Port
	inst.RemoteEndpoint = fmt.Sprintf("%s:%d", node, r.Port)
	if !b.Tunnel {
		// directDial: the gateway dials the job host itself. Only meaningful when
		// the job shares the host, which the site has to guarantee.
		return node, r.Port, nil
	}
	port, err := b.ensureTunnel(ctx, inst, node, r.Port)
	if err != nil {
		return "", 0, err
	}
	return "127.0.0.1", port, nil
}

// ensureTunnel reuses a live tunnel or starts one on the instance's login-node
// port.
func (b *Backend) ensureTunnel(ctx context.Context, inst *model.Instance, node string, jobPort int) (int, error) {
	if inst.Port <= 0 {
		return 0, errors.New("sge: the instance record has no login-node port for the tunnel")
	}
	if b.Ports == nil {
		return 0, errors.New("sge: the service needs a login-node port pool for its tunnel, but none is wired on this host")
	}
	if t := b.tunnelFor(inst.ID); t != nil && t.Alive() && b.dialable(inst.Port, time.Second) {
		return inst.Port, nil
	}
	tun := &Tunnel{
		LocalPort:    inst.Port,
		Node:         node,
		RemoteHost:   "127.0.0.1",
		RemotePort:   jobPort,
		User:         b.SSHUser,
		SSHBin:       b.SSH,
		ExtraArgs:    b.SSHArgs,
		ReadyTimeout: b.TunnelReadyTimeout,
		Dial:         b.Dialable,
	}
	if err := b.startTunnel(ctx, inst, tun); err != nil {
		return 0, err
	}
	return inst.Port, nil
}

func (b *Backend) startTunnel(ctx context.Context, inst *model.Instance, tun *Tunnel) error {
	factory := b.NewTunnel
	if factory == nil {
		factory = defaultTunnelFactory
	}
	started, err := factory(ctx, tun)
	if err != nil {
		return fmt.Errorf("ssh -L to %s:%d: %w", tun.Node, tun.RemotePort, err)
	}
	if started == nil {
		return errors.New("sge: the tunnel factory returned no tunnel")
	}
	b.setTunnel(inst.ID, started)
	// Recorded so a restarted control plane can reap its predecessor's ssh
	// instead of leaving one orphan per restart.
	if pid := started.PID(); pid > 0 {
		_ = rendezvous.WriteString(inst.ControlDir, "tunnel.pid", strconv.Itoa(pid), 0o644)
	}
	return nil
}

// Adopt re-establishes reachability after a control-plane restart.
//
// The job is *not* killed just because this process lost its handle: the shared
// filesystem preserved everything the job needs, and its ssh child often
// survived in its own process group. Killing is reserved for the one case where
// it is right — a job holding a compute slot with no reachable route.
func (b *Backend) Adopt(ctx context.Context, inst *model.Instance) (string, int, error) {
	if inst == nil {
		return "", 0, errors.New("sge: nil instance")
	}
	obs, err := b.Probe(ctx, inst)
	if err != nil {
		return "", 0, err
	}
	switch obs.Phase {
	case runtime.PhaseQueued:
		return "", 0, runtime.ErrNotYetRunning
	case runtime.PhaseGone, runtime.PhaseFailed:
		return "", 0, fmt.Errorf("job %s: %s", inst.BackendRef, strings.TrimSpace(obs.Note+" "+string(obs.Phase)))
	}
	r, err := rendezvous.ReadReady(inst.ControlDir)
	if err != nil {
		// Placed but not ready yet: the fast tick will try again.
		return "", 0, runtime.ErrNotYetRunning
	}
	if r.Incarnation != inst.Incarnation {
		// A stale incarnation holds a slot with no legitimate route.
		_ = b.StopInstance(ctx, inst)
		return "", 0, fmt.Errorf("job %s published ready.json for incarnation %s, record has %s",
			inst.BackendRef, r.Incarnation, inst.Incarnation)
	}
	if r.State != "ready" || r.Port <= 0 {
		return "", 0, runtime.ErrNotYetRunning
	}
	node, err := b.nodeFor(inst, r)
	if err != nil {
		return "", 0, err
	}
	inst.Node = node
	inst.JobPort = r.Port
	inst.RemoteEndpoint = fmt.Sprintf("%s:%d", node, r.Port)
	if !b.Tunnel {
		return node, r.Port, nil
	}
	if inst.Port <= 0 {
		return "", 0, errors.New("sge: the instance record has no login-node port for the tunnel")
	}
	// Fast path: the forward may still be alive.
	if b.dialable(inst.Port, time.Second) {
		return "127.0.0.1", inst.Port, nil
	}

	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", 0, ctx.Err()
			case <-time.After(b.RetryDelay):
			}
		}
		b.stopStaleTunnel(inst)
		tun := &Tunnel{
			LocalPort:    inst.Port,
			Node:         node,
			RemoteHost:   "127.0.0.1",
			RemotePort:   r.Port,
			User:         b.SSHUser,
			SSHBin:       b.SSH,
			ExtraArgs:    b.SSHArgs,
			ReadyTimeout: b.TunnelReadyTimeout,
			Dial:         b.Dialable,
		}
		if err := b.startTunnel(ctx, inst, tun); err == nil {
			return "127.0.0.1", inst.Port, nil
		} else {
			last = err
		}
	}
	// Unreachable after three attempts: release the slot rather than burn it.
	_ = b.StopInstance(ctx, inst)
	return "", 0, fmt.Errorf("unreachable after control-plane restart: %w", last)
}

// stopStaleTunnel kills the ssh a previous control plane left behind, but only
// when the pid can be proven to be ours: it must be alive and its command line
// must carry both a local forward and this instance's loopback port.
func (b *Backend) stopStaleTunnel(inst *model.Instance) {
	raw, err := rendezvous.Read(inst.ControlDir, "tunnel.pid")
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || pid <= 0 {
		return
	}
	cmdline, err := b.readCmdline(pid)
	if err != nil {
		return // dead, or not ours to inspect
	}
	if !strings.Contains(cmdline, "-L") || !strings.Contains(cmdline, fmt.Sprintf("127.0.0.1:%d:", inst.Port)) {
		return
	}
	_ = b.killPID(pid, syscall.SIGTERM)
}

func (b *Backend) readCmdline(pid int) (string, error) {
	if b.ReadCmdline != nil {
		return b.ReadCmdline(pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(data), "\x00", " "), nil
}

func (b *Backend) killPID(pid int, sig syscall.Signal) error {
	if b.KillPID != nil {
		return b.KillPID(pid, sig)
	}
	return syscall.Kill(pid, sig)
}

// terminalState reports whether the job has recorded a state no later transition
// follows.
func terminalState(s string) bool {
	switch strings.TrimSpace(s) {
	case "exited", "failed", "deleted":
		return true
	default:
		return false
	}
}
