package sge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// runningQstat is a one-job `qstat -xml` document for the given job id.
func runningQstat(id string) string {
	return fmt.Sprintf(`<queue_info><job_list state="running">
    <JB_job_number>%s</JB_job_number>
    <state>r</state>
    <queue_name>batch.q@node001</queue_name>
    <slots>4</slots>
  </job_list></queue_info>`, id)
}

func queuedQstat(id string) string {
	return fmt.Sprintf(`<job_info><job_list state="pending">
    <JB_job_number>%s</JB_job_number>
    <state>qw</state>
    <queue_name>batch.q@node001</queue_name>
  </job_list></job_info>`, id)
}

func publishReady(t *testing.T, inst *model.Instance, port int) {
	t.Helper()
	r := rendezvous.Ready{
		Version:     1,
		Incarnation: inst.Incarnation,
		State:       "ready",
		Host:        "127.0.0.1",
		Port:        port,
		PID:         4242,
		Node:        "node001",
		PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, r); err != nil {
		t.Fatal(err)
	}
	if err := rendezvous.WriteString(inst.ControlDir, "node", "node001", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rendezvous.WriteString(inst.ControlDir, "state", "ready", 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAwaitsPlacementThenReady is the queue-is-not-slow-start rule: the readiness
// budget starts only when qstat first reports `r`.
func TestAwaitsPlacementThenReady(t *testing.T) {
	b := testBackend(t)
	b.PollEvery = time.Nanosecond // bypass the one-qstat-per-tick cache
	b.ReadyWait = 30 * time.Millisecond
	inst, _ := writableSpec(t)
	inst.BackendRef = "12345"
	// The queue wait alone is longer than the readiness budget; if the budget
	// started at the beginning this would fail.
	b.Runner.(*fakeRunner).on("qstat",
		cmdResult{out: queuedQstat("12345")},
		cmdResult{out: queuedQstat("12345")},
		cmdResult{out: queuedQstat("12345")},
		cmdResult{out: runningQstat("12345")},
	)
	publishReady(t, &inst, 41000)

	ready, err := b.awaitReady(t.Context(), &inst, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Port != 41000 || ready.Incarnation != inst.Incarnation {
		t.Fatalf("ready: %+v", ready)
	}
}

func TestEndpointFailsFastOnEqw(t *testing.T) {
	b := testBackend(t)
	inst, _ := writableSpec(t)
	inst.BackendRef = "1837446"
	doc, err := os.ReadFile("testdata/qstat-eqw.xml")
	if err != nil {
		t.Fatal(err)
	}
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: string(doc)})
	b.InvalidateCache()

	_, err = b.awaitReady(t.Context(), &inst, time.Second)
	if err == nil || !strings.Contains(err.Error(), "Eqw") {
		t.Fatalf("Eqw must fail immediately with the state: %v", err)
	}
	if n := b.Runner.(*fakeRunner).count("qdel"); n != 0 {
		t.Fatalf("Eqw is a decision, not a retry loop: qdel calls %d", n)
	}
}

func TestEndpointFailsOnTerminalRendezvousState(t *testing.T) {
	b := testBackend(t)
	inst, _ := writableSpec(t)
	if err := rendezvous.WriteString(inst.ControlDir, "state", "failed", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rendezvous.WriteString(inst.ControlDir, "exit_code", "6", 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := b.awaitReady(t.Context(), &inst, time.Second)
	if err == nil || !strings.Contains(err.Error(), "6") {
		t.Fatalf("the exit code must be reported: %v", err)
	}
}

func TestSuspendedIsNotAFailure(t *testing.T) {
	b := testBackend(t)
	b.PollEvery = time.Nanosecond
	inst, _ := writableSpec(t)
	inst.BackendRef = "12345"
	suspendedDoc := `<queue_info><job_list state="suspended">
    <JB_job_number>12345</JB_job_number><state>s</state>
  </job_list></queue_info>`
	b.Runner.(*fakeRunner).on("qstat",
		cmdResult{out: queuedQstat("12345")},
		cmdResult{out: suspendedDoc},
		cmdResult{out: suspendedDoc},
		cmdResult{out: runningQstat("12345")},
	)
	publishReady(t, &inst, 41001)

	ready, err := b.awaitReady(t.Context(), &inst, 5*time.Second)
	if err != nil {
		t.Fatalf("a suspended job must keep waiting, not fail: %v", err)
	}
	if ready.Port != 41001 {
		t.Fatalf("ready: %+v", ready)
	}
}

func TestErrNotYetRunningWhenBudgetZero(t *testing.T) {
	b := testBackend(t)
	inst, _ := writableSpec(t)
	inst.BackendRef = "12345"
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: queuedQstat("12345")})
	b.InvalidateCache()

	_, err := b.awaitReady(t.Context(), &inst, 0)
	if !errors.Is(err, runtime.ErrNotYetRunning) {
		t.Fatalf("expected ErrNotYetRunning, got %v", err)
	}
	if n := b.Runner.(*fakeRunner).count("qstat"); n != 1 {
		t.Fatalf("a zero budget means one probe, got %d", n)
	}
}

func TestQueueTimeoutFails(t *testing.T) {
	b := testBackend(t)
	b.PollEvery = time.Nanosecond
	b.QueueTimeout = 20 * time.Millisecond
	inst, _ := writableSpec(t)
	inst.BackendRef = "12345"
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: queuedQstat("12345")})
	b.Runner.(*fakeRunner).on("qdel", cmdResult{out: "deleted\n"})

	_, err := b.awaitReady(t.Context(), &inst, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("expected a queue timeout, got %v", err)
	}
	if n := b.Runner.(*fakeRunner).count("qdel"); n != 1 {
		t.Fatalf("a job that never ran must release its slot: qdel calls %d", n)
	}
}

// TestEndpointReturnsLoginPort is the vertical slice: submit -> ready.json ->
// tunnel -> Endpoint answers with the loopback login port.
func TestEndpointReturnsLoginPort(t *testing.T) {
	b := testBackend(t)
	runner := b.Runner.(*fakeRunner)
	runner.on("qsub", cmdResult{out: "12345\n"})
	runner.on("qstat", cmdResult{out: runningQstat("12345")})
	dialer := newFakeDialer()
	b.Dialable = dialer.dial
	factory := &fakeTunnelFactory{onStart: func(tun *Tunnel) { dialer.set(tun.LocalPort, true) }}
	b.NewTunnel = factory.factory

	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)
	h, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	publishReady(t, &inst, 41002)

	ready, err := h.Endpoint(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Host != "127.0.0.1" || ready.Port != 24105 {
		t.Fatalf("endpoint must be the login-node tunnel port: %+v", ready)
	}
	if factory.startCount() != 1 {
		t.Fatalf("tunnel starts: %d", factory.startCount())
	}
	if factory.last.Node != "node001" || factory.last.RemotePort != 41002 {
		t.Fatalf("tunnel target: %+v", factory.last)
	}

	// A second Endpoint reuses the live tunnel.
	if _, err := h.Endpoint(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	if factory.startCount() != 1 {
		t.Fatalf("a live tunnel must be reused, starts=%d", factory.startCount())
	}

	// Stop qdels and tears the tunnel down.
	runner.on("qdel", cmdResult{out: "deleted\n"})
	if err := h.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runner.count("qdel") != 1 {
		t.Fatal("handle Stop must qdel")
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("Stop must be idempotent: %v", err)
	}
}

func TestEndpointRejectsUnusableNode(t *testing.T) {
	b := testBackend(t)
	b.PollEvery = time.Nanosecond
	inst, _ := writableSpec(t)
	inst.BackendRef = "12345"
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: runningQstat("12345")})
	publishReady(t, &inst, 41003)
	if err := rendezvous.WriteString(inst.ControlDir, "node", "-oProxyCommand=/bin/sh", 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := b.openEndpoint(t.Context(), &inst, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unusable node name") {
		t.Fatalf("a hostile node name must be rejected: %v", err)
	}
}

// ── adoption ─────────────────────────────────────────────────────────────

func adoptBackend(t *testing.T) (*Backend, *model.Instance, *fakeDialer, *fakeTunnelFactory) {
	t.Helper()
	b := testBackend(t)
	b.PollEvery = time.Nanosecond
	b.RetryDelay = time.Millisecond
	inst, _ := writableSpec(t)
	inst.BackendRef = "12345"
	inst.Port = 24105
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: runningQstat("12345")})
	b.Runner.(*fakeRunner).on("qdel", cmdResult{out: "deleted\n"})
	publishReady(t, &inst, 41002)
	dialer := newFakeDialer()
	b.Dialable = dialer.dial
	// The factory does not make the port dialable, so adopt goes through the
	// rebuild path unless a test says the old forward survived.
	factory := &fakeTunnelFactory{}
	b.NewTunnel = factory.factory
	return b, &inst, dialer, factory
}

func TestAdoptFastPath(t *testing.T) {
	b, inst, dialer, factory := adoptBackend(t)
	dialer.set(inst.Port, true) // the ssh child survived the restart

	host, port, err := b.Adopt(t.Context(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port != inst.Port {
		t.Fatalf("endpoint: %s:%d", host, port)
	}
	if factory.startCount() != 0 {
		t.Fatalf("nothing to rebuild: starts=%d", factory.startCount())
	}
	if inst.RemoteEndpoint != "node001:41002" || inst.JobPort != 41002 {
		t.Fatalf("record: %+v", inst)
	}
}

func TestAdoptRebuildsTunnel(t *testing.T) {
	b, inst, _, factory := adoptBackend(t)
	host, port, err := b.Adopt(t.Context(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port != inst.Port {
		t.Fatalf("endpoint: %s:%d", host, port)
	}
	if factory.startCount() != 1 {
		t.Fatalf("tunnel rebuilds: %d", factory.startCount())
	}
	if factory.last.LocalPort != inst.Port || factory.last.RemotePort != 41002 || factory.last.Node != "node001" {
		t.Fatalf("tunnel: %+v", factory.last)
	}
}

func TestAdoptKillsStaleTunnelPID(t *testing.T) {
	b, inst, _, factory := adoptBackend(t)
	pidPath := inst.ControlDir + "/tunnel.pid"
	if err := rendezvous.WriteString(inst.ControlDir, "tunnel.pid", "5150", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatal(err)
	}
	var killed []int
	var sigs []syscall.Signal
	b.KillPID = func(pid int, sig syscall.Signal) error {
		killed = append(killed, pid)
		sigs = append(sigs, sig)
		return nil
	}

	// A pid whose command line is not ours must never be killed.
	b.ReadCmdline = func(pid int) (string, error) { return "/usr/bin/vim notes.txt", nil }
	b.stopStaleTunnel(inst)
	if len(killed) != 0 {
		t.Fatalf("killed a pid we cannot prove is ours: %v", killed)
	}

	// A pid carrying our forward and port is ours to reap.
	b.ReadCmdline = func(pid int) (string, error) {
		return fmt.Sprintf("ssh -N -L 127.0.0.1:%d:127.0.0.1:41002 -- node001", inst.Port), nil
	}
	b.stopStaleTunnel(inst)
	if len(killed) != 1 || killed[0] != 5150 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("stale tunnel: killed=%v sigs=%v", killed, sigs)
	}

	// Adopt performs that sweep before every rebuild attempt.
	factory.fail = errors.New("ssh: refused")
	if _, _, err := b.Adopt(t.Context(), inst); err == nil {
		t.Fatal("expected adopt to give up")
	}
	if factory.startCount() != 3 {
		t.Fatalf("rebuild attempts: %d", factory.startCount())
	}
	if len(killed) == 1 {
		t.Fatalf("adopt must sweep the stale tunnel before each attempt: %v", killed)
	}
}

func TestAdoptGivesUpAndQdels(t *testing.T) {
	b, inst, _, factory := adoptBackend(t)
	factory.fail = errors.New("ssh: connect to host node001 port 22: refused")
	_, _, err := b.Adopt(t.Context(), inst)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("expected an unreachable error, got %v", err)
	}
	if factory.startCount() != 3 {
		t.Fatalf("rebuild attempts: %d", factory.startCount())
	}
	if n := b.Runner.(*fakeRunner).count("qdel"); n != 1 {
		t.Fatalf("a job with no route must release its slot: qdel calls %d", n)
	}
}

func TestAdoptStaleIncarnationFails(t *testing.T) {
	b, inst, _, _ := adoptBackend(t)
	stale := rendezvous.Ready{
		Version: 1, Incarnation: "previous-incarnation", State: "ready",
		Host: "127.0.0.1", Port: 41002, Node: "node001", PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, stale); err != nil {
		t.Fatal(err)
	}
	_, _, err := b.Adopt(t.Context(), inst)
	if err == nil || !strings.Contains(err.Error(), "incarnation") {
		t.Fatalf("expected an incarnation error, got %v", err)
	}
	if n := b.Runner.(*fakeRunner).count("qdel"); n != 1 {
		t.Fatalf("a stale incarnation holds a slot for nothing: qdel calls %d", n)
	}
}

func TestAdoptQueuedIsNotYetRunning(t *testing.T) {
	b, inst, _, _ := adoptBackend(t)
	b.Runner.(*fakeRunner).set("qstat", cmdResult{out: queuedQstat("12345")})
	b.InvalidateCache()
	_, _, err := b.Adopt(t.Context(), inst)
	if !errors.Is(err, runtime.ErrNotYetRunning) {
		t.Fatalf("expected ErrNotYetRunning, got %v", err)
	}
}

func TestAdoptRunningButNotReadyWaits(t *testing.T) {
	b, inst, _, _ := adoptBackend(t)
	if err := os.Remove(inst.ControlDir + "/ready.json"); err != nil {
		t.Fatal(err)
	}
	_, _, err := b.Adopt(t.Context(), inst)
	if !errors.Is(err, runtime.ErrNotYetRunning) {
		t.Fatalf("a running job without ready.json is still starting: %v", err)
	}
}

// TestAliveRequiresTheTunnel pins "alive means reachable": a job whose forward
// died is not a working instance.
func TestAliveRequiresTheTunnel(t *testing.T) {
	b := testBackend(t)
	b.Runner.(*fakeRunner).on("qsub", cmdResult{out: "12345\n"})
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: runningQstat("12345")})
	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)
	h, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Alive() {
		t.Fatal("a running job with no tunnel must be alive")
	}
	tun := &fakeTunnel{alive: false}
	b.setTunnel(inst.ID, tun)
	if h.Alive() {
		t.Fatal("a dead tunnel means the instance is unreachable")
	}
}

// ── tunnel ───────────────────────────────────────────────────────────────

func TestTunnelArgsGolden(t *testing.T) {
	tun := &Tunnel{
		LocalPort:    24105,
		Node:         "node001",
		RemoteHost:   "127.0.0.1",
		RemotePort:   41002,
		User:         "dw",
		ExtraArgs:    []string{"-i", "/etc/deepwarper/secrets/sge_id_ed25519"},
		ReadyTimeout: time.Second,
	}
	args, err := tun.Args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-N",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ClearAllForwardings=no",
		"-L", "127.0.0.1:24105:127.0.0.1:41002",
		"-i", "/etc/deepwarper/secrets/sge_id_ed25519",
		"--", "dw@node001",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ssh argv:\n got %q\nwant %q", args, want)
	}
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"-R", "GatewayPorts", "ProxyCommand"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("argv must not contain %q: %s", forbidden, joined)
		}
	}
}

func TestTunnelRejectsHostileNodeName(t *testing.T) {
	for _, node := range []string{
		"-oProxyCommand=touch /tmp/pwned",
		" node001",
		"a/b",
		"node001;rm -rf /",
		"",
	} {
		tun := &Tunnel{LocalPort: 24105, Node: node, RemoteHost: "127.0.0.1", RemotePort: 41002, ReadyTimeout: time.Millisecond}
		if _, err := tun.Args(); err == nil {
			t.Fatalf("%q must be rejected", node)
		}
		// Start must fail before forking ssh.
		if err := tun.Start(context.Background()); err == nil {
			t.Fatalf("%q: Start must fail", node)
		}
	}
	for _, ok := range []string{"node001", "node-001.batch.example.com", "10.0.0.7", "::1"} {
		if !ValidHost(ok) {
			t.Fatalf("%q must be accepted", ok)
		}
	}
}

func TestTunnelWaitsUntilDialable(t *testing.T) {
	var mu sync.Mutex
	up := false
	go func() {
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		up = true
		mu.Unlock()
	}()
	dial := func(host string, port int, timeout time.Duration) bool {
		mu.Lock()
		defer mu.Unlock()
		return up
	}
	start := time.Now()
	if err := waitDialable(context.Background(), 24105, 5*time.Second, dial, nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("Start must not report success before the forward works (waited %s)", elapsed)
	}
}

func TestWaitDialableGivesUp(t *testing.T) {
	dial := func(host string, port int, timeout time.Duration) bool { return false }
	err := waitDialable(context.Background(), 24105, 100*time.Millisecond, dial, nil)
	if err == nil || !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("expected a timeout, got %v", err)
	}
}

func TestWaitDialableStopsWhenSSHExits(t *testing.T) {
	dial := func(host string, port int, timeout time.Duration) bool { return false }
	exited := func() bool { return true }
	err := waitDialable(context.Background(), 24105, 5*time.Second, dial, exited)
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("expected a fast failure, got %v", err)
	}
}

func TestTunnelStopIsIdempotent(t *testing.T) {
	tun := &Tunnel{}
	if err := tun.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tun.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tun.Alive() {
		t.Fatal("an unstarted tunnel is not alive")
	}
}

var _ = agent.Spec{}
