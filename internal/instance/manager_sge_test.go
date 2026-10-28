package instance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/ports"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// fakeScheduler is a scheduler-backed backend: it has a placement phase, its
// idle reaper is off, it adopts after a restart, and it can renew leases.
type fakeScheduler struct {
	mu       sync.Mutex
	obs      runtime.Observation
	probeErr error
	handle   runtime.Handle
	started  []agent.Spec
	stops    []*model.Instance
	adopts   []*model.Instance
	renews   []*model.Instance
	renewed  bool
	renewErr error
	lease    string
	leaseOK  bool

	idleReap bool
	adoptErr error
}

func (f *fakeScheduler) Name() string { return "sge" }

func (f *fakeScheduler) Start(ctx context.Context, spec agent.Spec) (runtime.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, spec)
	if f.handle == nil {
		return nil, errors.New("no handle scripted")
	}
	return f.handle, nil
}

func (f *fakeScheduler) Stop(ctx context.Context, inst *model.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, inst)
	return nil
}

func (f *fakeScheduler) Probe(ctx context.Context, inst *model.Instance) (runtime.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.obs, f.probeErr
}

func (f *fakeScheduler) LaunchBudget() time.Duration { return 0 }
func (f *fakeScheduler) PollInterval() time.Duration { return time.Second }
func (f *fakeScheduler) IdleReapAllowed() bool       { return f.idleReap }

func (f *fakeScheduler) Adopt(ctx context.Context, inst *model.Instance) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopts = append(f.adopts, inst)
	if f.adoptErr != nil {
		return "", 0, f.adoptErr
	}
	return "127.0.0.1", inst.Port, nil
}

func (f *fakeScheduler) Renew(ctx context.Context, inst *model.Instance) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews = append(f.renews, inst)
	if f.renewErr != nil {
		return false, f.renewErr
	}
	if !f.renewed {
		return false, nil
	}
	inst.LeaseExpiresAt = time.Now().UTC().Add(time.Hour)
	inst.LeaseWarnedAt = time.Time{}
	return true, nil
}

func (f *fakeScheduler) LeaseWarning(inst *model.Instance) (string, bool) {
	return f.lease, f.leaseOK
}

func (f *fakeScheduler) setObservation(obs runtime.Observation) {
	f.mu.Lock()
	f.obs = obs
	f.mu.Unlock()
}

func (f *fakeScheduler) adoptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.adopts)
}

func (f *fakeScheduler) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func (f *fakeScheduler) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stops)
}

// schedulerManager builds a manager whose active backend is the fake scheduler,
// with the real local backend registered next to it so routing can be tested.
func schedulerManager(t *testing.T, fake *fakeScheduler) *Manager {
	t.Helper()
	cfg := testConfig(t)
	cfg.Runtime.Default = "sge"
	pool := ports.New(cfg.Runtime.Local.PortRange[0], cfg.Runtime.Local.PortRange[1])
	m, err := newManagerWithBackends(cfg, pool,
		map[string]runtime.Backend{"local": &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}}, "sge": fake},
		fake)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sgeRecord(t *testing.T, m *Manager, state model.State) *model.Instance {
	t.Helper()
	dir := filepath.Join(m.cfg.Server.DataDir, "control", "alice-default")
	inst := &model.Instance{
		ID:           "alice-default",
		User:         "alice",
		Slug:         "alice",
		Distribution: "default",
		State:        state,
		Backend:      "sge",
		BackendRef:   "12345",
		Incarnation:  "inc1",
		UnitName:     "dw-alice-3f2a1b9c",
		ControlDir:   dir,
		LogPath:      filepath.Join(dir, "job.log"),
		Port:         24105,
		StartedAt:    time.Now().UTC(),
		LastActiveAt: time.Now().UTC(),
	}
	saveInstance(t, m, inst)
	return inst
}

func TestEnsureQueuedReturnsImmediately(t *testing.T) {
	fake := &fakeScheduler{handle: &fakeHandle{
		ref:     "12345",
		limiter: "sge",
		endErr:  runtime.ErrNotYetRunning,
		ready:   &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: 24105},
	}}
	m := schedulerManager(t, fake)

	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != model.StateQueued {
		t.Fatalf("state: %s", inst.State)
	}
	if inst.Backend != "sge" || inst.BackendRef != "12345" {
		t.Fatalf("record: backend=%q ref=%q", inst.Backend, inst.BackendRef)
	}
	if inst.PID != 0 {
		t.Fatalf("a job has no login-node pid: %d", inst.PID)
	}
	if !strings.HasSuffix(inst.LogPath, "job.log") {
		t.Fatalf("the job log must live in the control dir: %q", inst.LogPath)
	}
	if fake.startCount() != 1 {
		t.Fatalf("starts: %d", fake.startCount())
	}
	// The login-node port stays reserved for the queued instance.
	if err := m.ports.Reserve(inst.Port, "someone-else"); err == nil {
		t.Fatal("a queued instance must keep its port")
	}
}

func TestEnsureIdempotentOnQueued(t *testing.T) {
	fake := &fakeScheduler{handle: &fakeHandle{ref: "12345", limiter: "sge", endErr: runtime.ErrNotYetRunning}}
	m := schedulerManager(t, fake)
	first, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != model.StateQueued {
		t.Fatalf("second ensure: %+v", second)
	}
	if fake.startCount() != 1 {
		t.Fatalf("a queued instance must not be submitted twice: %d", fake.startCount())
	}
}

func TestPromoteQueuedToRunning(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning, Source: "qstat"}}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateQueued)
	ready := rendezvous.Ready{
		Version: 1, Incarnation: inst.Incarnation, State: "ready",
		Host: "127.0.0.1", Port: inst.Port, Node: "node001", PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, ready); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateRunning {
		t.Fatalf("state: %s", got.State)
	}
	if got.Endpoint != fmt.Sprintf("127.0.0.1:%d", inst.Port) {
		t.Fatalf("endpoint: %s", got.Endpoint)
	}
	if fake.adoptCount() != 1 {
		t.Fatalf("adopt calls: %d", fake.adoptCount())
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err == nil {
		t.Fatal("a running instance must hold its port")
	}
}

func TestPromoteWithoutRouteYetKeepsQueued(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}, adoptErr: runtime.ErrNotYetRunning}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateQueued)
	ready := rendezvous.Ready{Version: 1, Incarnation: inst.Incarnation, State: "ready", Host: "127.0.0.1", Port: inst.Port}
	if err := rendezvous.WriteReady(inst.ControlDir, ready); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateQueued {
		t.Fatalf("not-yet-running is not a failure: %s", got.State)
	}
}

// TestStartingRecordAfterRestartAdoptsRoute is the crash-restart case: the record
// says starting, this process holds no handle, and the job is running. Adopt must
// re-establish the route instead of resubmitting or killing the job.
func TestStartingRecordAfterRestartAdoptsRoute(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateStarting)
	ready := rendezvous.Ready{
		Version: 1, Incarnation: inst.Incarnation, State: "ready", Host: "127.0.0.1",
		Port: inst.Port, Node: "node001", PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, ready); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateRunning {
		t.Fatalf("state: %s", got.State)
	}
	if fake.adoptCount() != 1 {
		t.Fatalf("adopt calls: %d", fake.adoptCount())
	}
	if fake.stopCount() != 0 || fake.startCount() != 0 {
		t.Fatalf("a restart must not stop or resubmit a live job: stops=%d starts=%d", fake.stopCount(), fake.startCount())
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err == nil {
		t.Fatal("an adopted instance must hold its port")
	}
}

func TestQueuedKeepsPositionNote(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseQueued, QueuePos: 7, Note: "queued: position 7 in batch.q"}}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateStarting)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateQueued {
		t.Fatalf("state: %s", got.State)
	}
	if !strings.Contains(got.Error, "position 7") {
		t.Fatalf("the queue position must be visible: %q", got.Error)
	}
}

// TestSuspendedIsNeverReapedAndNeverResubmitted pins ADR-015: a preempted job is
// paused work, not a dead instance.
func TestSuspendedIsNeverReapedAndNeverResubmitted(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseSuspended, Note: "job is suspended by the scheduler"}}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateSuspended)
	inst.LastActiveAt = time.Now().UTC().Add(-10 * time.Hour)
	inst.IdleTTL = "1m"
	saveInstance(t, m, inst)

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateSuspended {
		t.Fatalf("state: %s", got.State)
	}
	if fake.stopCount() != 0 {
		t.Fatalf("a suspended instance must never be stopped by the reaper: %d", fake.stopCount())
	}
	if fake.startCount() != 0 {
		t.Fatalf("a suspended instance must never be resubmitted: %d", fake.startCount())
	}
	if fake.adoptCount() != 0 {
		t.Fatal("a suspended instance needs no new route")
	}
}

func TestSweepSkippedOnSchedulerByDefault(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateRunning)
	inst.LastActiveAt = time.Now().UTC().Add(-10 * time.Hour)
	inst.IdleTTL = "1m"
	saveInstance(t, m, inst)

	if err := m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.stopCount() != 0 {
		t.Fatalf("the SGE reaper is off by default: stops=%d", fake.stopCount())
	}

	fake.idleReap = true
	if err := m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.stopCount() != 1 {
		t.Fatalf("a site that opts in must be honoured: stops=%d", fake.stopCount())
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("state: %s", got.State)
	}
}

func TestSweepUsesNewestActivity(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}, idleReap: true}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateRunning)
	inst.LastActiveAt = time.Now().UTC().Add(-10 * time.Hour)
	inst.IdleTTL = "30m"
	saveInstance(t, m, inst)
	m.Touch(inst)
	if err := m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.stopCount() != 0 {
		t.Fatalf("an instance with fresh traffic is not idle: stops=%d", fake.stopCount())
	}
}

func TestRenewTickDedupsByLease(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}, renewed: true}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateRunning)
	for i := 0; i < 2; i++ {
		if err := m.RenewTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.renews) != 2 {
		t.Fatalf("renew calls: %d", len(fake.renews))
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.LeaseExpiresAt.IsZero() {
		t.Fatal("a successful renewal must be persisted")
	}
}

func TestRenewFailureIsLoggedNotFatal(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}, renewErr: errors.New("qalter: denied")}
	m := schedulerManager(t, fake)
	var logged []string
	m.SetLogf(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	inst := sgeRecord(t, m, model.StateRunning)
	if err := m.RenewTick(context.Background()); err != nil {
		t.Fatalf("a renewal failure must not fail the tick: %v", err)
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "denied") {
		t.Fatalf("the failure must be logged: %v", logged)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateRunning {
		t.Fatalf("state must not change: %s", got.State)
	}
}

func TestLeaseWarningDedupedByRecord(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}, lease: "the job's h_rt ceiling expires in 12m", leaseOK: true}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateRunning)
	if err := m.RenewTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, _ := m.store.LoadByID(inst.ID)
	if !strings.Contains(first.Error, "h_rt ceiling") || first.LeaseWarnedAt.IsZero() {
		t.Fatalf("warning not recorded: %+v", first)
	}
	firstWarned := first.LeaseWarnedAt
	// A second tick must not repeat the warning for the same window.
	first.Error = ""
	saveInstance(t, m, first)
	if err := m.RenewTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.Error != "" {
		t.Fatalf("warning repeated: %q", got.Error)
	}
	if !got.LeaseWarnedAt.Equal(firstWarned) {
		t.Fatal("LeaseWarnedAt must be stable within one window")
	}
}

func TestStopRoutesByRecordBackend(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}}
	local := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}}
	cfg := testConfig(t)
	pool := ports.New(cfg.Runtime.Local.PortRange[0], cfg.Runtime.Local.PortRange[1])
	m, err := newManagerWithBackends(cfg, pool,
		map[string]runtime.Backend{"local": local, "sge": fake}, local)
	if err != nil {
		t.Fatal(err)
	}
	inst := sgeRecord(t, m, model.StateRunning)
	if err := m.Stop(context.Background(), "alice", "default"); err != nil {
		t.Fatal(err)
	}
	if fake.stopCount() != 1 {
		t.Fatalf("an sge record must be stopped through the sge backend: %d", fake.stopCount())
	}
	if len(local.stops) != 0 {
		t.Fatalf("the local backend must not see an sge instance: %d", len(local.stops))
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("state: %s", got.State)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err != nil {
		t.Fatalf("a stopped instance must release its port: %v", err)
	}
}

func TestProbeRoutesByRecordBackend(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}}
	local := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseInactive, Detail: "process not found"}}
	cfg := testConfig(t)
	pool := ports.New(cfg.Runtime.Local.PortRange[0], cfg.Runtime.Local.PortRange[1])
	m, err := newManagerWithBackends(cfg, pool,
		map[string]runtime.Backend{"local": local, "sge": fake}, local)
	if err != nil {
		t.Fatal(err)
	}
	inst := sgeRecord(t, m, model.StateRunning)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateRunning {
		t.Fatalf("the sge backend's verdict must win: %s", got.State)
	}
}

func TestNewInstancesUseDefaultBackend(t *testing.T) {
	fake := &fakeScheduler{handle: &fakeHandle{
		ref: "12345", limiter: "sge", endErr: runtime.ErrNotYetRunning,
	}}
	local := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}}
	cfg := testConfig(t)
	cfg.Runtime.Default = "sge"
	pool := ports.New(cfg.Runtime.Local.PortRange[0], cfg.Runtime.Local.PortRange[1])
	m, err := newManagerWithBackends(cfg, pool,
		map[string]runtime.Backend{"local": local, "sge": fake}, fake)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Backend != "sge" {
		t.Fatalf("backend: %q", inst.Backend)
	}
	if fake.startCount() != 1 || len(local.starts) != 0 {
		t.Fatalf("starts: sge=%d local=%d", fake.startCount(), len(local.starts))
	}
	if strings.Contains(inst.UnitName, ".service") {
		t.Fatalf("an SGE job name is not a unit name: %q", inst.UnitName)
	}
}

func TestPolicyGatesTheSchedulerReaper(t *testing.T) {
	if _, ok := runtime.Backend(&fakeScheduler{}).(runtime.Policy); !ok {
		t.Fatal("the fake scheduler must expose a Policy")
	}
	local := &fakeBackend{}
	if _, ok := runtime.Backend(local).(runtime.Policy); ok {
		t.Fatal("the local backend must keep the default lifecycle policy")
	}
}

// TestEnsurePersistsFarSideOfTheTunnel keeps the operator-visible fields honest:
// the gateway dials Port, and Node/JobPort/RemoteEndpoint describe the job side.
func TestEnsurePersistsFarSideOfTheTunnel(t *testing.T) {
	ready := &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: 24105, Node: "node001"}
	fake := &fakeScheduler{handle: &fakeHandle{
		ref: "12345", limiter: "sge", node: "node001", jobPort: 41002, ready: ready,
	}}
	m := schedulerManager(t, fake)
	// The pooled port is unknown up front; make the handle publish it.
	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Node != "node001" || inst.JobPort != 41002 || inst.RemoteEndpoint != "node001:41002" {
		t.Fatalf("record: node=%q jobPort=%d remote=%q", inst.Node, inst.JobPort, inst.RemoteEndpoint)
	}
	// ready.Port equals the pooled port only if the fake reused it; either way the
	// record's Port is what the gateway dials and the endpoint must agree.
	if inst.Endpoint != fmt.Sprintf("127.0.0.1:%d", inst.Port) {
		t.Fatalf("endpoint %q must match port %d", inst.Endpoint, inst.Port)
	}
}

// TestEnsureAdoptsThePortTheJobPublished covers directDial-style backends: a port
// the control plane did not allocate must replace the pooled one instead of
// leaving a reserved port pointing nowhere.
func TestEnsureAdoptsThePortTheJobPublished(t *testing.T) {
	ready := &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: 33999}
	fake := &fakeScheduler{handle: &fakeHandle{ref: "12345", limiter: "sge", ready: ready}}
	m := schedulerManager(t, fake)
	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Port != 33999 {
		t.Fatalf("port: %d", inst.Port)
	}
	if inst.Endpoint != "127.0.0.1:33999" {
		t.Fatalf("endpoint: %q", inst.Endpoint)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err == nil {
		t.Fatal("the adopted port must be held by the instance")
	}
	// The pooled port must be back in the pool, not leaked.
	if port, err := m.ports.Acquire("probe"); err != nil {
		t.Fatalf("pool exhausted: %v", err)
	} else {
		m.ports.Release(port)
	}
}

// TestQueuedWithHandlePromotesThroughTheHandle is the trap where ready.json's port
// is the JOB's port: after Ensure returned queued, the fast tick must promote via
// the handle (which owns the tunnel) and keep the login port the gateway dials.
func TestQueuedWithHandlePromotesThroughTheHandle(t *testing.T) {
	fake := &fakeScheduler{obs: runtime.Observation{Phase: runtime.PhaseRunning}}
	m := schedulerManager(t, fake)
	inst := sgeRecord(t, m, model.StateQueued)
	jobReady := rendezvous.Ready{
		Version: 1, Incarnation: inst.Incarnation, State: "ready",
		Host: "127.0.0.1", Port: 41002, Node: "node001", PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, jobReady); err != nil {
		t.Fatal(err)
	}
	// The in-process handle owns the tunnel and answers with the login port.
	handleReady := &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: inst.Port}
	h := &fakeHandle{ref: "12345", limiter: "sge", node: "node001", jobPort: 41002, ready: handleReady}
	m.handles[inst.ID] = h

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateRunning {
		t.Fatalf("state: %s", got.State)
	}
	if got.Endpoint != fmt.Sprintf("127.0.0.1:%d", inst.Port) {
		t.Fatalf("endpoint must be the login port, got %q", got.Endpoint)
	}
	if got.JobPort != 41002 || got.RemoteEndpoint != "node001:41002" {
		t.Fatalf("far side not recorded: jobPort=%d remote=%q", got.JobPort, got.RemoteEndpoint)
	}
	if fake.adoptCount() != 0 {
		t.Fatalf("a handle in this process needs no adopt: %d", fake.adoptCount())
	}
}
