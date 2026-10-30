package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

type fakeBackend struct {
	obs      runtime.Observation
	probeErr error
	handle   runtime.Handle
	startErr error
	starts   []agent.Spec
	stops    []*model.Instance
	// calls records the order of start/stop calls for tests that care that a
	// previous process was stopped before a new one was started.
	calls []string
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Start(ctx context.Context, spec agent.Spec) (runtime.Handle, error) {
	f.calls = append(f.calls, "start")
	f.starts = append(f.starts, spec)
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.handle, nil
}

func (f *fakeBackend) Stop(ctx context.Context, inst *model.Instance) error {
	f.calls = append(f.calls, "stop")
	f.stops = append(f.stops, inst)
	return nil
}

func (f *fakeBackend) Probe(ctx context.Context, inst *model.Instance) (runtime.Observation, error) {
	return f.obs, f.probeErr
}

type fakeHandle struct {
	ref      string
	limiter  string
	pid      int
	pidStart uint64
	command  []string
	node     string
	jobPort  int
	ready    *rendezvous.Ready
	endErr   error
	stopped  bool
}

func (h *fakeHandle) DescribeEndpoint() (string, int) { return h.node, h.jobPort }

func (h *fakeHandle) Ref() string       { return h.ref }
func (h *fakeHandle) PID() int          { return h.pid }
func (h *fakeHandle) PIDStart() uint64  { return h.pidStart }
func (h *fakeHandle) Limiter() string   { return h.limiter }
func (h *fakeHandle) Command() []string { return append([]string{}, h.command...) }
func (h *fakeHandle) Alive() bool       { return !h.stopped }
func (h *fakeHandle) Endpoint(ctx context.Context, timeout time.Duration) (*rendezvous.Ready, error) {
	if h.endErr != nil {
		return nil, h.endErr
	}
	return h.ready, nil
}
func (h *fakeHandle) Stop(ctx context.Context) error {
	h.stopped = true
	return nil
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.DataDir = filepath.Join(root, "data")
	cfg.Server.Listen = "127.0.0.1:7658"
	cfg.Routing.HostSuffix = "localhost"
	cfg.Routing.PublicScheme = "http"
	cfg.Runtime.DefaultDistribution = "default"
	cfg.Runtime.DSHBin = "/bin/true"
	cfg.Runtime.DWBin = "/bin/true"
	cfg.Runtime.ReadyTimeout = "90s"
	cfg.Runtime.ScanInterval = "30s"
	cfg.Runtime.Heartbeat = "30s"
	cfg.Runtime.Local.PortRange = [2]int{24100, 24120}
	cfg.Runtime.Local.Limiter = "auto"
	cfg.Runtime.Local.TasksMax = 512
	cfg.Runtime.Local.SystemdStartTimeout = "10s"
	cfg.Runtime.Local.StopGrace = "20s"
	cfg.Runtime.Local.UnitStdio = "log"
	cfg.Distributions = map[string]config.DistSpec{
		"default": {Name: "default", Version: "1.0.0", ProfileName: "default", Path: dist},
	}
	return cfg
}

func newTestManager(t *testing.T, backend runtime.Backend) *Manager {
	t.Helper()
	m, err := newManager(testConfig(t), backend)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func saveInstance(t *testing.T, m *Manager, inst *model.Instance) {
	t.Helper()
	if err := m.store.Save(inst); err != nil {
		t.Fatal(err)
	}
}

func activeInstance(t *testing.T, m *Manager) *model.Instance {
	t.Helper()
	dir := filepath.Join(m.cfg.Server.DataDir, "control", "alice-default")
	inst := &model.Instance{
		ID:           "alice-default",
		User:         "alice",
		Slug:         "alice",
		Distribution: "default",
		State:        model.StateStarting,
		Incarnation:  "inc1",
		BackendRef:   "dw-alice-3f2a1b9c.service",
		UnitName:     "dw-alice-3f2a1b9c.service",
		ControlDir:   dir,
		LogPath:      filepath.Join(m.cfg.Server.DataDir, "logs", "alice-default.log"),
		Port:         24105,
		StartedAt:    time.Now().UTC(),
	}
	saveInstance(t, m, inst)
	return inst
}

func TestReconcileAdoptsActiveUnitWithReady(t *testing.T) {
	m := newTestManager(t, &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}})
	inst := activeInstance(t, m)
	r := rendezvous.Ready{
		Version: 1, Incarnation: inst.Incarnation, State: "ready",
		Host: "127.0.0.1", Port: inst.Port, PID: 999, Profile: "default",
		PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, r); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := m.store.LoadByID(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateRunning {
		t.Fatalf("state: %s", got.State)
	}
	if got.Endpoint != "127.0.0.1:24105" {
		t.Fatalf("endpoint: %s", got.Endpoint)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err == nil {
		t.Fatal("port should be reserved for the adopted instance")
	}
}

func TestReconcileKeepsStartingWhileActivating(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActivating}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStarting {
		t.Fatalf("state: %s", got.State)
	}
	if len(fake.stops) != 0 {
		t.Fatalf("activating must not be stopped: %#v", fake.stops)
	}
	// A starting instance owns its port: otherwise a new Ensure could pick the
	// same port before the adopted instance binds it.
	if err := m.ports.Reserve(inst.Port, "someone-else"); err == nil {
		t.Fatal("a starting instance's port must stay reserved")
	}
	if owner, ok := m.ports.Owner(inst.Port); !ok || owner != inst.ID+"/"+inst.Incarnation {
		t.Fatalf("owner: %q/%v", owner, ok)
	}
}

func TestReconcileSettlesFailedUnitWithVerdict(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{
		Phase:   runtime.PhaseFailed,
		Detail:  "result=oom-kill exit=137",
		Verdict: "KILL oom-kill",
	}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StateRunning
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("state: %s", got.State)
	}
	if !strings.Contains(got.Error, "oom-kill") {
		t.Fatalf("error: %q", got.Error)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err != nil {
		t.Fatalf("dead instance must release its port: %v", err)
	}
}

func TestReconcileUsesPIDPathForLegacyRecords(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive, Source: "pid"}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.BackendRef = ""
	inst.PID = os.Getpid()
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStarting {
		t.Fatalf("live legacy pid must keep its record, got %s", got.State)
	}

	fake.obs = runtime.Observation{Phase: runtime.PhaseInactive, Source: "pid", Detail: "process not found"}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("dead legacy pid: %s", got.State)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err != nil {
		t.Fatalf("port should be released: %v", err)
	}
}

func TestReconcileFailsStuckStarting(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.StartedAt = time.Now().UTC().Add(-2 * time.Minute)
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateFailed {
		t.Fatalf("state: %s", got.State)
	}
	if !strings.Contains(got.Error, "ready timeout") {
		t.Fatalf("error: %q", got.Error)
	}
	if len(fake.stops) != 1 {
		t.Fatalf("stuck instance should be stopped once, got %d", len(fake.stops))
	}
}

func TestStopAfterRestartUsesBackend(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StateRunning
	saveInstance(t, m, inst)
	if err := m.Stop(context.Background(), "alice", "default"); err != nil {
		t.Fatal(err)
	}
	if len(fake.stops) != 1 {
		t.Fatalf("backend.Stop calls: %d", len(fake.stops))
	}
	if fake.stops[0].BackendRef != inst.BackendRef {
		t.Fatalf("stop used %q", fake.stops[0].BackendRef)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("state: %s", got.State)
	}
}

func TestEnsurePassesUnitNameAndPersistsRef(t *testing.T) {
	h := &fakeHandle{
		ref:      "dw-alice-0000abcd.service",
		limiter:  "systemd",
		pid:      1234,
		pidStart: 5678,
		command:  []string{"systemd-run", "--user"},
		ready:    &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: 24105},
	}
	fake := &fakeBackend{handle: h}
	m := newTestManager(t, fake)
	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(inst.UnitName, ".service") {
		t.Fatalf("unit name: %q", inst.UnitName)
	}
	if inst.BackendRef != h.ref || inst.Limiter != "systemd" {
		t.Fatalf("ref/limiter: %q/%q", inst.BackendRef, inst.Limiter)
	}
	if inst.PID != 1234 || inst.PIDStart != 5678 {
		t.Fatalf("pid/pidstart: %d/%d", inst.PID, inst.PIDStart)
	}
	if len(inst.Command) == 0 {
		t.Fatal("command not persisted")
	}
	if len(fake.starts) != 1 {
		t.Fatalf("starts: %d", len(fake.starts))
	}
	if fake.starts[0].UnitName != inst.UnitName {
		t.Fatalf("spec unit: %q want %q", fake.starts[0].UnitName, inst.UnitName)
	}
	if fake.starts[0].ReadyTimeout != "90s" {
		t.Fatalf("spec ready timeout: %q", fake.starts[0].ReadyTimeout)
	}
}

func TestEnsureReadyFailureStopsUnitAndReleasesPort(t *testing.T) {
	h := &fakeHandle{
		ref:     "dw-alice-0000abcd.service",
		limiter: "systemd",
		endErr:  errors.New("ready exploded"),
	}
	m := newTestManager(t, &fakeBackend{handle: h})
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err == nil {
		t.Fatal("expected ensure failure")
	}
	if !h.stopped {
		t.Fatal("failed ensure must stop the handle")
	}
	got, err := m.store.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateFailed {
		t.Fatalf("state: %s", got.State)
	}
	if err := m.ports.Reserve(got.Port, "someone-else"); err != nil {
		t.Fatalf("failed ensure must release the port: %v", err)
	}
}

func TestShutdownDoesNotStopInstances(t *testing.T) {
	fake := &fakeBackend{}
	m := newTestManager(t, fake)
	m.Shutdown(context.Background())
	if len(fake.stops) != 0 {
		t.Fatalf("shutdown must not stop instances: %d", len(fake.stops))
	}
}

// flakyStopBackend fails Stop a configurable number of times before it
// succeeds, which is what the Stopping retry path must tolerate.
type flakyStopBackend struct {
	fakeBackend
	stopErrs int
}

func (f *flakyStopBackend) Stop(ctx context.Context, inst *model.Instance) error {
	f.stops = append(f.stops, inst)
	if f.stopErrs > 0 {
		f.stopErrs--
		return errors.New("stop failed")
	}
	return nil
}

func TestEnsureRejectsUnsafeIdentity(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	for _, user := range []string{"", "../alice", "a/b", `a\b`, "alice.", "a\x00b", ".."} {
		if _, err := m.Ensure(context.Background(), Request{User: user, Distribution: "default"}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("user %q: %v", user, err)
		}
	}
	if list, err := m.store.List(); err != nil || len(list) != 0 {
		t.Fatalf("rejected identities must not create records: %v %v", list, err)
	}
}

func TestEnsureRequiresKnownUserInLoginMode(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	m.cfg.Auth.UsersDir = filepath.Join(t.TempDir(), "users")
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("error: %v", err)
	}
	if list, err := m.store.List(); err != nil || len(list) != 0 {
		t.Fatalf("unknown user must not create a record: %v %v", list, err)
	}
}

func TestEnsureRejectsWorkspaceOutsideRoots(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	outside := t.TempDir()
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Workspace: outside}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("outside workspace: %v", err)
	}
	workspaces := filepath.Join(m.cfg.Server.DataDir, "workspaces")
	if err := os.MkdirAll(workspaces, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Workspace: workspaces}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("workspace root must be rejected: %v", err)
	}
	other := filepath.Join(workspaces, "bob")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Workspace: other}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("another user's workspace must be rejected: %v", err)
	}
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Workspace: "relative/path"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("relative workspace: %v", err)
	}
}

func TestEnsureRejectsStoppingWithBusy(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	inst := activeInstance(t, m)
	inst.State = model.StateStopping
	saveInstance(t, m, inst)
	_, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("error: %v", err)
	}
	var busy *BusyError
	if !errors.As(err, &busy) || busy.State != model.StateStopping {
		t.Fatalf("busy error: %#v", err)
	}
}

func TestEnsureReturnsFreshPendingWithoutStarting(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	inst.StartedAt = time.Time{}
	inst.CreatedAt = time.Now().UTC()
	saveInstance(t, m, inst)
	got, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Incarnation != inst.Incarnation || got.State != model.StatePending {
		t.Fatalf("got %s/%s", got.Incarnation, got.State)
	}
	if len(fake.starts) != 0 {
		t.Fatalf("a pending launch must not start again: %d", len(fake.starts))
	}
}

func TestEnsureReclaimsStalePending(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	inst.StartedAt = time.Time{}
	inst.CreatedAt = time.Now().UTC().Add(-2 * m.pendingLimit())
	saveInstance(t, m, inst)
	got, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != inst.ID {
		t.Fatalf("record id must be reused: %q want %q", got.ID, inst.ID)
	}
	if got.Incarnation == inst.Incarnation {
		t.Fatal("stale pending must get a new incarnation")
	}
	if got.State != model.StateRunning {
		t.Fatalf("state: %s", got.State)
	}
	if len(fake.starts) != 1 {
		t.Fatalf("starts: %d", len(fake.starts))
	}
}

func TestEnsureStalePendingStopsPreviousProcess(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	inst.StartedAt = time.Time{}
	inst.CreatedAt = time.Now().UTC().Add(-2 * m.pendingLimit())
	saveInstance(t, m, inst)
	if err := m.ports.Reserve(inst.Port, portOwner(inst)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.stops) != 1 || fake.stops[0].Incarnation != inst.Incarnation {
		t.Fatalf("the stale launch must be stopped once: %#v", fake.stops)
	}
	if strings.Join(fake.calls, ",") != "stop,start" {
		t.Fatalf("stop must happen before the new start: %#v", fake.calls)
	}
	if got.Incarnation == inst.Incarnation {
		t.Fatal("a new launch needs a new incarnation")
	}
	if got.State != model.StateRunning {
		t.Fatalf("state: %s", got.State)
	}
	if owner, ok := m.ports.Owner(got.Port); !ok || owner != portOwner(got) {
		t.Fatalf("new port owner: %q/%v", owner, ok)
	}
}

func TestEnsureStalePendingStopFailureKeepsStopping(t *testing.T) {
	fake := &flakyStopBackend{
		fakeBackend: fakeBackend{handle: readyHandle()},
		stopErrs:    1,
	}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	inst.StartedAt = time.Time{}
	inst.CreatedAt = time.Now().UTC().Add(-2 * m.pendingLimit())
	saveInstance(t, m, inst)
	if err := m.ports.Reserve(inst.Port, portOwner(inst)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err == nil {
		t.Fatal("a failed quiesce must fail the launch")
	} else if !strings.Contains(err.Error(), "reuse stop failed") {
		t.Fatalf("error: %v", err)
	}
	got, err := m.store.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateStopping {
		t.Fatalf("a failed quiesce must stay retryable, got %s", got.State)
	}
	if !strings.Contains(got.Error, "reuse stop failed") {
		t.Fatalf("record error: %q", got.Error)
	}
	if got.StopAttempts != 1 {
		t.Fatalf("stop attempts: %d", got.StopAttempts)
	}
	if owner, ok := m.ports.Owner(got.Port); !ok || owner != portOwner(got) {
		t.Fatalf("port must stay reserved: %q/%v", owner, ok)
	}
	if len(fake.starts) != 0 {
		t.Fatalf("no process may be started after a failed quiesce: %d", len(fake.starts))
	}

	// Reconcile retries the stop and settles the record.
	got.LastStopAttemptAt = time.Now().UTC().Add(-time.Minute)
	if err := m.store.Save(got); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("state after retry: %s", got.State)
	}
}

func TestReconcileSeedsLastActiveForAdoptedRunning(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StateRunning
	inst.LastActiveAt = time.Now().UTC().Add(-24 * time.Hour)
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := m.store.LoadByID(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(m.LastActive(got)); age > time.Minute {
		t.Fatalf("LastActive was not seeded on adoption: %s ago", age)
	}
}

func TestEnsureStartFailureMarksFailedAndReleasesPort(t *testing.T) {
	fake := &fakeBackend{startErr: errors.New("spawn exploded")}
	m := newTestManager(t, fake)
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err == nil {
		t.Fatal("expected launch failure")
	}
	got, err := m.store.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateFailed {
		t.Fatalf("state: %s", got.State)
	}
	if err := m.ports.Reserve(got.Port, "someone-else"); err != nil {
		t.Fatalf("failed launch must release its port: %v", err)
	}
}

func TestEnsureAbortStopFailureKeepsStoppingAndPort(t *testing.T) {
	fake := &flakyStopBackend{
		fakeBackend: fakeBackend{startErr: errors.New("spawn exploded")},
		stopErrs:    1,
	}
	m := newTestManager(t, fake)
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err == nil {
		t.Fatal("expected launch failure")
	}
	got, err := m.store.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateStopping {
		t.Fatalf("failed stop must keep the record retryable, got %s", got.State)
	}
	if !strings.Contains(got.Error, "stop failed") {
		t.Fatalf("error: %q", got.Error)
	}
	if owner, ok := m.ports.Owner(got.Port); !ok || owner != got.ID+"/"+got.Incarnation {
		t.Fatalf("port must stay reserved for this incarnation: %q/%v", owner, ok)
	}
}

func TestReconcilePromotesPendingWithReady(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActivating}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	saveInstance(t, m, inst)
	ready := rendezvous.Ready{
		Version: 1, Incarnation: inst.Incarnation, State: "ready",
		Host: "127.0.0.1", Port: inst.Port, PID: 999, Profile: "default",
		PublishedAt: time.Now().UTC(),
	}
	if err := rendezvous.WriteReady(inst.ControlDir, ready); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateRunning || got.Endpoint != "127.0.0.1:24105" {
		t.Fatalf("got %s %s", got.State, got.Endpoint)
	}
	if len(fake.starts) != 0 {
		t.Fatal("reconcile must never start a process")
	}
}

func TestReconcileLeavesPendingWithoutPromotingToStarting(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActivating}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	inst.StartedAt = time.Time{}
	inst.CreatedAt = time.Now().UTC()
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StatePending {
		t.Fatalf("reconcile must not move Pending to %s", got.State)
	}
}

func TestReconcileAbortsPendingGone(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseInactive, Source: "pid", Detail: "process not found"}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateFailed {
		t.Fatalf("state: %s", got.State)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err != nil {
		t.Fatalf("port must be released: %v", err)
	}
}

func TestReconcileAbortsPendingAfterLimit(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActivating}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StatePending
	inst.StartedAt = time.Time{}
	inst.CreatedAt = time.Now().UTC().Add(-2 * m.pendingLimit())
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateFailed {
		t.Fatalf("state: %s", got.State)
	}
	if !strings.Contains(got.Error, "launch did not start") {
		t.Fatalf("error: %q", got.Error)
	}
}

func TestReconcileReservesPendingAndStoppingPorts(t *testing.T) {
	m := newTestManager(t, &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActivating}})
	pending := activeInstance(t, m)
	pending.State = model.StatePending
	pending.CreatedAt = time.Now().UTC()
	saveInstance(t, m, pending)
	stopping := &model.Instance{
		ID: "bob-default", User: "bob", Slug: "bob", Distribution: "default",
		State: model.StateStopping, Incarnation: "incb", Port: 24106,
		ControlDir: filepath.Join(m.cfg.Server.DataDir, "control", "bob-default"),
		LogPath:    filepath.Join(m.cfg.Server.DataDir, "logs", "bob-default.log"),
		CreatedAt:  time.Now().UTC(),
	}
	saveInstance(t, m, stopping)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, inst := range []*model.Instance{pending, stopping} {
		if owner, ok := m.ports.Owner(inst.Port); !ok || owner != inst.ID+"/"+inst.Incarnation {
			t.Fatalf("%s port %d owner %q/%v", inst.ID, inst.Port, owner, ok)
		}
	}
}

func TestReconcileRetriesStoppingThenSettles(t *testing.T) {
	fake := &flakyStopBackend{
		fakeBackend: fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}},
		stopErrs:    1,
	}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StateStopping
	inst.LastStopAttemptAt = time.Now().UTC().Add(-time.Minute)
	saveInstance(t, m, inst)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.LoadByID(inst.ID)
	if got.State != model.StateStopping {
		t.Fatalf("failed retry must stay Stopping, got %s", got.State)
	}
	if got.StopAttempts != 1 {
		t.Fatalf("attempts: %d", got.StopAttempts)
	}
	if owner, ok := m.ports.Owner(inst.Port); !ok || owner != inst.ID+"/"+inst.Incarnation {
		t.Fatalf("port must stay reserved: %q/%v", owner, ok)
	}

	// The next retry is only due after the minimum interval.
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.stops) != 1 {
		t.Fatalf("retry ran before the interval: %d", len(fake.stops))
	}
	got.LastStopAttemptAt = time.Now().UTC().Add(-time.Minute)
	if err := m.store.Save(got); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = m.store.LoadByID(inst.ID)
	if got.State != model.StateStopped {
		t.Fatalf("state: %s", got.State)
	}
	if err := m.ports.Reserve(inst.Port, "someone-else"); err != nil {
		t.Fatalf("settled port must be released: %v", err)
	}
}

func TestReconcileMarksOrphanSuspectedOnce(t *testing.T) {
	fake := &flakyStopBackend{
		fakeBackend: fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive}},
		stopErrs:    100,
	}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.State = model.StateStopping
	inst.LastStopAttemptAt = time.Now().UTC().Add(-time.Minute)
	saveInstance(t, m, inst)
	cur := inst
	for i := 0; i < 5; i++ {
		if err := m.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		cur, _ = m.store.LoadByID(inst.ID)
		cur.LastStopAttemptAt = time.Now().UTC().Add(-time.Minute)
		if err := m.store.Save(cur); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(cur.Error, "orphan suspected") != 1 {
		t.Fatalf("error: %q", cur.Error)
	}
}

func TestTokenRejectsControlDirOutsideDataDir(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	inst := &model.Instance{ID: "alice-default", ControlDir: "/tmp/dw-elsewhere"}
	if _, err := m.Token(inst); err == nil {
		t.Fatal("outside control dir was accepted")
	}
}

// lockedBackend is safe for concurrent Manager calls; the plain fake appends
// to slices without a lock and would race under -race.
type lockedBackend struct {
	mu     sync.Mutex
	obs    runtime.Observation
	handle runtime.Handle
}

func (b *lockedBackend) Name() string { return "locked" }

func (b *lockedBackend) Start(ctx context.Context, spec agent.Spec) (runtime.Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handle, nil
}

func (b *lockedBackend) Stop(ctx context.Context, inst *model.Instance) error { return nil }

func (b *lockedBackend) Probe(ctx context.Context, inst *model.Instance) (runtime.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.obs, nil
}

type safeHandle struct {
	ready *rendezvous.Ready
}

func (h *safeHandle) Ref() string                    { return "" }
func (h *safeHandle) PID() int                       { return 0 }
func (h *safeHandle) PIDStart() uint64               { return 0 }
func (h *safeHandle) Limiter() string                { return "none" }
func (h *safeHandle) Command() []string              { return nil }
func (h *safeHandle) Alive() bool                    { return true }
func (h *safeHandle) Stop(ctx context.Context) error { return nil }
func (h *safeHandle) Endpoint(ctx context.Context, timeout time.Duration) (*rendezvous.Ready, error) {
	return h.ready, nil
}

func TestManagerConcurrentLifecycle(t *testing.T) {
	backend := &lockedBackend{
		obs:    runtime.Observation{Phase: runtime.PhaseActive},
		handle: &safeHandle{ready: &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: 24105}},
	}
	m := newTestManager(t, backend)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			if _, err := m.Ensure(ctx, Request{User: "alice", Distribution: "default"}); err != nil &&
				!errors.Is(err, ErrBusy) && !errors.Is(err, ErrUnknownInstance) {
				t.Errorf("ensure: %v", err)
			}
		}()
		go func() { defer wg.Done(); _ = m.Reconcile(ctx) }()
		go func() { defer wg.Done(); _ = m.Sweep(ctx) }()
		go func() {
			defer wg.Done()
			if err := m.Stop(ctx, "alice", "default"); err != nil && !errors.Is(err, ErrUnknownInstance) {
				t.Errorf("stop: %v", err)
			}
		}()
	}
	wg.Wait()
	list, err := m.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("records: %d", len(list))
	}
	if list[0].State != model.StateStopped && list[0].State != model.StateRunning && list[0].State != model.StateFailed {
		t.Fatalf("settled state: %s", list[0].State)
	}
}

func TestEnsureReusesLegacyRecordPaths(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	home := filepath.Join(m.cfg.Server.DataDir, "homes", "alice")
	ws := filepath.Join(m.cfg.Server.DataDir, "workspaces", "alice")
	ctl := filepath.Join(m.cfg.Server.DataDir, "control", "alice-default")
	lg := filepath.Join(m.cfg.Server.DataDir, "logs", "alice-default.log")
	for _, d := range []string{home, ws, ctl, filepath.Dir(lg)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &model.Instance{
		ID: "alice-default", User: "alice", Slug: "alice", Distribution: "default",
		State: model.StateStopped, Home: home, Workspace: ws, ControlDir: ctl, LogPath: lg,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := m.store.Save(legacy); err != nil {
		t.Fatal(err)
	}
	got, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alice-default" || got.Slug != "alice" {
		t.Fatalf("id/slug: %q/%q", got.ID, got.Slug)
	}
	if got.Home != home || got.Workspace != ws || got.ControlDir != ctl || got.LogPath != lg {
		t.Fatalf("legacy paths must be reused: %#v", got)
	}
	if _, err := os.Stat(m.store.InstancePath("alice", "default")); err == nil {
		t.Fatal("a second record file was created for a legacy instance")
	}
	if _, err := os.Stat(filepath.Join(m.cfg.Server.DataDir, "instances", "alice-default.yaml")); err != nil {
		t.Fatalf("legacy record file must stay in place: %v", err)
	}
}

func TestReconcileAdoptsLegacyRecordWithoutIncarnation(t *testing.T) {
	fake := &fakeBackend{obs: runtime.Observation{Phase: runtime.PhaseActive, PID: 4321}}
	m := newTestManager(t, fake)
	inst := activeInstance(t, m)
	inst.Incarnation = ""
	inst.State = model.StateStarting
	inst.PID = 4321
	inst.StartedAt = time.Now().UTC().Add(-time.Minute)
	saveInstance(t, m, inst)
	ready := rendezvous.Ready{
		Version: 1, Incarnation: "legacyinc", State: "ready",
		Host: "127.0.0.1", Port: inst.Port, PID: 4321, Profile: "default",
		PublishedAt: time.Now().UTC(),
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
	if got.Incarnation != "legacyinc" {
		t.Fatalf("incarnation must be adopted: %q", got.Incarnation)
	}
	if owner, ok := m.ports.Owner(got.Port); !ok || owner != got.ID+"/legacyinc" {
		t.Fatalf("port must be re-owned under the adopted incarnation: %q/%v", owner, ok)
	}
}

func TestPublicAuthorityUsesEffectivePublicPort(t *testing.T) {
	cases := []struct {
		slug   string
		suffix string
		port   int
		scheme string
		want   string
	}{
		{slug: "alice", suffix: "localhost", port: 80, scheme: "http", want: "alice.localhost"},
		{slug: "alice", suffix: "localhost", port: 443, scheme: "https", want: "alice.localhost"},
		{slug: "alice", suffix: "localhost", port: 7658, scheme: "http", want: "alice.localhost:7658"},
		{slug: "alice", suffix: "example.com", port: 8443, scheme: "https", want: "alice.example.com:8443"},
		{slug: "alice", suffix: "example.com", port: 0, scheme: "https", want: "alice.example.com"},
	}
	for _, tc := range cases {
		if got := publicAuthority(tc.slug, tc.suffix, tc.port, tc.scheme); got != tc.want {
			t.Errorf("publicAuthority(%q,%q,%d,%q) = %q, want %q", tc.slug, tc.suffix, tc.port, tc.scheme, got, tc.want)
		}
	}
}

func TestEnsureUsesConfiguredPublicPort(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	m.cfg.Routing.PublicPort = 443
	m.cfg.Routing.PublicScheme = "https"
	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.PublicHost != "alice.localhost" {
		t.Fatalf("public host: %q", inst.PublicHost)
	}

	m2 := newTestManager(t, &fakeBackend{handle: readyHandle()})
	m2.cfg.Routing.PublicPort = 8443
	m2.cfg.Routing.PublicScheme = "https"
	inst2, err := m2.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if inst2.PublicHost != "alice.localhost:8443" {
		t.Fatalf("public host with custom port: %q", inst2.PublicHost)
	}
}

func TestEnsureRejectsTombstonedUserInLoginMode(t *testing.T) {
	m := newTestManager(t, &fakeBackend{handle: readyHandle()})
	users := filepath.Join(t.TempDir(), "users")
	m.cfg.Auth.UsersDir = users
	if err := auth.Create(users, "alice", "first-secret"); err != nil {
		t.Fatal(err)
	}
	if err := auth.Delete(users, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("tombstoned user: %v", err)
	}
	if list, err := m.store.List(); err != nil || len(list) != 0 {
		t.Fatalf("tombstoned user must not create a record: %v %v", list, err)
	}
	if err := auth.Create(users, "alice", "second-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err != nil {
		t.Fatalf("recreated user: %v", err)
	}
}
