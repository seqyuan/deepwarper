package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
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
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Start(ctx context.Context, spec agent.Spec) (runtime.Handle, error) {
	f.starts = append(f.starts, spec)
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.handle, nil
}

func (f *fakeBackend) Stop(ctx context.Context, inst *model.Instance) error {
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
	if err := m.ports.Reserve(inst.Port, "someone-else"); err != nil {
		t.Fatalf("port must not be reserved before ready: %v", err)
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
