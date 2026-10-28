package sge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
)

// ── fakes ────────────────────────────────────────────────────────────────
//
// Every SGE test runs without a cluster and without ssh: the Runner records argv
// and returns scripted output, the tunnel factory returns a tunnel whose
// liveness the test controls, and clock/dial/proc are injected.

type cmdCall struct {
	name string
	args []string
}

func (c cmdCall) joined() string { return c.name + " " + strings.Join(c.args, " ") }

type cmdResult struct {
	out string
	err error
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []cmdCall
	results map[string][]cmdResult
	last    map[string]cmdResult
	// onRun lets a test make something happen while a command "runs": a job that
	// starts and fails before qsub returns, for example.
	onRun func(cmdCall)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{results: map[string][]cmdResult{}, last: map[string]cmdResult{}}
}

// on scripts the next answers for one command, in order. Once exhausted the last
// answer repeats, which is what a steady scheduler state looks like.
func (f *fakeRunner) on(name string, results ...cmdResult) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[name] = append(f.results[name], results...)
	return f
}

// set replaces the scripted answers for one command (on appends).
func (f *fakeRunner) set(name string, results ...cmdResult) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[name] = append([]cmdResult{}, results...)
	delete(f.last, name)
	return f
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := cmdCall{name: name, args: append([]string{}, args...)}
	f.calls = append(f.calls, call)
	if f.onRun != nil {
		hook := f.onRun
		f.mu.Unlock()
		hook(call)
		f.mu.Lock()
	}
	if q := f.results[name]; len(q) > 0 {
		r := q[0]
		f.results[name] = q[1:]
		f.last[name] = r
		return r.out, r.err
	}
	if r, ok := f.last[name]; ok {
		return r.out, r.err
	}
	return "", nil
}

func (f *fakeRunner) callsFor(name string) []cmdCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cmdCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRunner) count(name string) int { return len(f.callsFor(name)) }

type fakePorts struct {
	mu   sync.Mutex
	next int
	used map[int]bool
}

func newFakePorts() *fakePorts { return &fakePorts{next: 24000, used: map[int]bool{}} }

func (p *fakePorts) Acquire(owner string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	p.used[p.next] = true
	return p.next, nil
}

func (p *fakePorts) Release(port int) {
	p.mu.Lock()
	delete(p.used, port)
	p.mu.Unlock()
}

// fakeDialer stands in for "something accepts connections on this loopback
// port". Tests flip ports on to simulate a tunnel becoming usable.
type fakeDialer struct {
	mu    sync.Mutex
	ports map[int]bool
}

func newFakeDialer() *fakeDialer { return &fakeDialer{ports: map[int]bool{}} }

func (d *fakeDialer) set(port int, up bool) {
	d.mu.Lock()
	d.ports[port] = up
	d.mu.Unlock()
}

func (d *fakeDialer) dial(host string, port int, timeout time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ports[port]
}

type fakeTunnel struct {
	mu      sync.Mutex
	pid     int
	alive   bool
	stopped bool
}

func (t *fakeTunnel) Alive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive
}

func (t *fakeTunnel) Stop(ctx context.Context) error {
	t.mu.Lock()
	t.stopped = true
	t.alive = false
	t.mu.Unlock()
	return nil
}

func (t *fakeTunnel) PID() int { return t.pid }

type fakeTunnelFactory struct {
	mu      sync.Mutex
	starts  int
	last    *Tunnel
	fail    error
	onStart func(t *Tunnel) // lets a test simulate the forward becoming usable
	tunnel  *fakeTunnel
}

func (f *fakeTunnelFactory) factory(ctx context.Context, t *Tunnel) (TunnelRuntime, error) {
	f.mu.Lock()
	f.starts++
	f.last = t
	fail := f.fail
	onStart := f.onStart
	f.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if onStart != nil {
		onStart(t)
	}
	ft := &fakeTunnel{pid: 4242, alive: true}
	f.mu.Lock()
	f.tunnel = ft
	f.mu.Unlock()
	return ft, nil
}

func (f *fakeTunnelFactory) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// ── fixture helpers ──────────────────────────────────────────────────────

const testImage = "/data/dw/images/dw-runtime.sif"

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func testBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	b := &Backend{Config: Config{
		Qsub:         "qsub",
		Qstat:        "qstat",
		Qdel:         "qdel",
		Qalter:       "qalter",
		Queue:        "batch.q",
		PE:           "smp",
		PEAccounting: "cores",

		SubmitDir:     filepath.Join(root, "submit"),
		RendezvousDir: root,
		ControlRoot:   filepath.Join(root, "control"),

		Tunnel:             true,
		TunnelReadyTimeout: time.Second,

		ApptainerImage:  testImage,
		MinHVmemPerSlot: 4 << 30,

		RenewBefore: 20 * time.Minute,
		RenewFor:    2 * time.Hour,
		WarnBefore:  30 * time.Minute,

		PollEvery: 2 * time.Second,
		Ports:     newFakePorts(),
	}}
	b.applyDefaults()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	b.Runner = newFakeRunner()
	return b
}

// staticSpec is a spec with site-shaped absolute paths and no filesystem side
// effects, so golden files do not contain a temp directory name.
func staticSpec() (model.Instance, string) {
	const root = "/data/dw"
	controlDir := root + "/control/alice-default"
	inst := model.Instance{
		ID:          "alice-default",
		User:        "alice",
		Slug:        "alice",
		Backend:     "sge",
		BackendRef:  "12345",
		Incarnation: "inc-7f3a1c2e",
		UnitName:    "dw-alice-7f3a1c2e",
		Home:        root + "/homes/alice",
		Workspace:   root + "/workspaces/alice",
		ControlDir:  controlDir,
		LogPath:     controlDir + "/job.log",
		Port:        24105,
		CPU:         4,
		Memory:      "16Gi",
		Walltime:    "8h",
		State:       model.StateRunning,
		StartedAt:   time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
	return inst, root + "/dist/default/1.0.0"
}

// writableSpec is staticSpec with a real, writable control directory: the
// readiness/adoption tests publish ready.json and state files into it.
func writableSpec(t *testing.T) (model.Instance, string) {
	t.Helper()
	inst, dist := staticSpec()
	inst.ControlDir = filepath.Join(t.TempDir(), "control", "alice-default")
	inst.LogPath = filepath.Join(inst.ControlDir, "job.log")
	if err := os.MkdirAll(inst.ControlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return inst, dist
}

func testSpec(t *testing.T, b *Backend) (model.Instance, string) {
	t.Helper()
	root := t.TempDir()
	controlDir := filepath.Join(root, "control", "alice-default")
	home := filepath.Join(root, "homes", "alice")
	workspace := filepath.Join(root, "workspaces", "alice")
	dist := filepath.Join(root, "dist", "default", "1.0.0")
	for _, dir := range []string{home, workspace, dist} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inst := model.Instance{
		ID:          "alice-default",
		User:        "alice",
		Slug:        "alice",
		Backend:     "sge",
		BackendRef:  "12345",
		Incarnation: "inc-7f3a1c2e",
		UnitName:    "dw-alice-7f3a1c2e",
		Home:        home,
		Workspace:   workspace,
		ControlDir:  controlDir,
		LogPath:     filepath.Join(controlDir, "job.log"),
		Port:        24105,
		CPU:         4,
		Memory:      "16Gi",
		Walltime:    "8h",
		State:       model.StateRunning,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return inst, dist
}

func hostSpec(inst model.Instance, distDir string, loginPort int) agent.Spec {
	return agent.Spec{
		InstanceID:   inst.ID,
		Incarnation:  inst.Incarnation,
		UnitName:     inst.UnitName,
		Profile:      "default",
		DshBin:       "/opt/dw/bin/dsh",
		Home:         inst.Home,
		Workspace:    inst.Workspace,
		DistPath:     distDir,
		DistVersion:  "1.0.0",
		ControlDir:   inst.ControlDir,
		LogPath:      inst.LogPath,
		PublicURL:    "https://alice.dsh.example.com/",
		TrustedHost:  "alice.dsh.example.com",
		Port:         loginPort,
		PatchContent: "patch",
		Heartbeat:    "30s",
		ReadyTimeout: "90s",
		CPU:          4,
		Memory:       "16Gi",
		Walltime:     "8h",
	}
}
