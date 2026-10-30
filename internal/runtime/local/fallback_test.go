package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

const fakeDwScript = `#!/bin/sh
# minimal ` + "`dw instance`" + ` stand-in used by the fallback tests
spec=""
while [ $# -gt 0 ]; do
  case "$1" in
    --spec) spec="$2"; shift 2 ;;
    *) shift ;;
  esac
done
dir=$(dirname "$spec")
inc=testinc
printf '%s\n' "$inc" > "$dir/incarnation"
printf 'starting\n' > "$dir/state"
printf '%s\n' "$$" > "$dir/pid"
cat > "$dir/ready.json" <<EOF
{"version":1,"incarnation":"$inc","state":"ready","host":"127.0.0.1","port":23456,"pid":$$,"profile":"default","published_at":"2024-01-01T00:00:00Z"}
EOF
printf 'ready\n' > "$dir/state"
trap 'printf "exited\n" > "$dir/state"; exit 0' TERM INT
while :; do sleep 1; done
`

func fakeDw(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/fakedw.sh"
	if err := os.WriteFile(path, []byte(fakeDwScript), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fallbackSpec(t *testing.T) agent.Spec {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"home", "ws", "control", "logs"} {
		if err := os.MkdirAll(root+"/"+d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return agent.Spec{
		InstanceID:  "alice-default",
		Incarnation: "testinc",
		UnitName:    "dw-alice-3f2a1b9c.service",
		Home:        root + "/home",
		Workspace:   root + "/ws",
		ControlDir:  root + "/control",
		LogPath:     root + "/logs/alice.log",
		Port:        23456,
		DshBin:      "/bin/true",
	}
}

func TestFallbackStartReadyStop(t *testing.T) {
	spec := fallbackSpec(t)
	b := New(Options{DwBin: fakeDw(t), Limiter: "none", ReadyWait: 5 * time.Second})
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.Limiter() != "none" {
		t.Fatalf("limiter: %q", h.Limiter())
	}
	if h.PID() <= 0 || h.PIDStart() == 0 {
		t.Fatalf("pid/pidstart: %d/%d", h.PID(), h.PIDStart())
	}
	ready, err := h.Endpoint(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Port != 23456 {
		t.Fatalf("ready port: %d", ready.Port)
	}
	pid := h.PID()
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("second stop must be idempotent: %v", err)
	}
	if processAlive(pid, 0) {
		t.Fatalf("process %d still alive after Stop", pid)
	}
}

func TestFallbackPrlimitArgv(t *testing.T) {
	spec := fallbackSpec(t)
	spec.Memory = "1Gi"
	f := &fakeRunner{paths: map[string]string{"prlimit": "/usr/bin/prlimit"}}
	b := New(Options{DwBin: fakeDw(t), Limiter: "prlimit", ReadyWait: 5 * time.Second, Run: f, PrlimitCPUSeconds: 0})
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	cmd := h.Command()
	if len(cmd) < 3 || cmd[0] != "prlimit" || cmd[1] != "--as=1073741824" || cmd[2] != "--" {
		t.Fatalf("unexpected prlimit argv: %#v", cmd)
	}
	if strings.Contains(strings.Join(cmd, " "), "--cpu=") {
		t.Fatalf("prlimitCPUSeconds=0 must not set RLIMIT_CPU: %#v", cmd)
	}
}

func TestStopRefusesReusedPID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	start := pidStartTime(pid)
	if start == 0 {
		t.Skip("no /proc starttime on this host")
	}
	if err := stopRecordedProcess(context.Background(), pid, start+1, ""); err != nil {
		t.Fatalf("mismatched pid must be a no-op, got %v", err)
	}
	if !processAlive(pid, 0) {
		t.Fatal("a mismatched pid must not be signalled")
	}
}

func TestStopRefusesPIDWithoutMatchingSpec(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	if err := stopRecordedProcess(context.Background(), pid, 0, "/definitely/not/our/spec.json"); err != nil {
		t.Fatalf("legacy mismatch must be a no-op, got %v", err)
	}
	if !processAlive(pid, 0) {
		t.Fatal("a legacy pid without a matching spec must not be signalled")
	}
}

func TestProcessEndpointFailsFastWhenAgentExits(t *testing.T) {
	spec := fallbackSpec(t)
	dw := filepath.Join(t.TempDir(), "dw-exit")
	if err := os.WriteFile(dw, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := New(Options{DwBin: dw, Limiter: "none", ReadyWait: 30 * time.Second})
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := h.Endpoint(context.Background(), 30*time.Second); err == nil {
		t.Fatal("an exited agent must fail the launch")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("process-mode endpoint did not fail fast: %s", elapsed)
	}
	_ = h.Stop(context.Background())
}

// startSleep starts a reaped `sleep` process in its own process group, so
// stopRecordedProcess can terminate it with the group signal it uses in
// production.
func startSleep(t *testing.T) (pid int, start uint64) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	start = pidStartTime(pid)
	if start == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skip("no /proc starttime on this host")
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		<-exited
	})
	return pid, start
}

func TestStopAndProbeUseControlDirPIDFallback(t *testing.T) {
	control := t.TempDir()
	pid, start := startSleep(t)
	for name, value := range map[string]string{
		"incarnation": "inc-fallback",
		"pid":         fmt.Sprint(pid),
		"pid_start":   fmt.Sprint(start),
	} {
		if err := rendezvous.WriteString(control, name, value, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "none", StopGrace: time.Second,
		Run: &fakeRunner{paths: map[string]string{}}})
	inst := &model.Instance{ID: "alice-default", Incarnation: "inc-fallback", ControlDir: control}

	obs, err := b.Probe(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Alive() || obs.PID != pid {
		t.Fatalf("control-dir pid was not probed: %#v", obs)
	}

	if err := b.Stop(context.Background(), inst); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if processAlive(pid, 0) {
		t.Fatalf("control-dir pid %d survived Stop", pid)
	}
	obs, err = b.Probe(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Alive() || obs.Phase != runtime.PhaseInactive {
		t.Fatalf("stopped pid must probe inactive: %#v", obs)
	}
}

func TestControlDirPIDIsIncarnationChecked(t *testing.T) {
	control := t.TempDir()
	pid, start := startSleep(t)
	for name, value := range map[string]string{
		"incarnation": "another-incarnation",
		"pid":         fmt.Sprint(pid),
		"pid_start":   fmt.Sprint(start),
	} {
		if err := rendezvous.WriteString(control, name, value, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "none", StopGrace: time.Second,
		Run: &fakeRunner{paths: map[string]string{}}})
	inst := &model.Instance{ID: "alice-default", Incarnation: "inc-wanted", ControlDir: control}

	obs, err := b.Probe(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Alive() {
		t.Fatalf("a foreign incarnation's pid must not be probed: %#v", obs)
	}
	if err := b.Stop(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	if !processAlive(pid, 0) {
		t.Fatal("a foreign incarnation's pid must not be signalled")
	}
}

func TestStopWithoutPIDOrSystemctlIsNoop(t *testing.T) {
	f := &fakeRunner{paths: map[string]string{}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "none", Run: f})
	inst := &model.Instance{
		ID:          "alice-default",
		Incarnation: "inc1",
		State:       model.StatePending,
		UnitName:    "dw-alice-0.service",
		ControlDir:  filepath.Join(t.TempDir(), "control"),
	}
	if err := b.Stop(context.Background(), inst); err != nil {
		t.Fatalf("a process-mode stop without a pid must not fail: %v", err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("systemd must not be invoked without systemctl: %#v", calls)
	}
}

func TestStopAdoptsPendingUnitWithoutBackendRef(t *testing.T) {
	f := &fakeRunner{
		paths: map[string]string{"systemctl": "/usr/bin/systemctl"},
		handler: func(name string, args []string) (RunResult, error) {
			if name == "systemctl" {
				if verb, _ := systemctlArgs(args); verb == "show" {
					return showOutput("inactive", 0, "success", 0), nil
				}
			}
			return RunResult{}, nil
		},
	}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", StopGrace: time.Second, Run: f})
	inst := &model.Instance{
		ID:          "alice-default",
		Incarnation: "inc1",
		State:       model.StatePending,
		UnitName:    "dw-alice-0.service",
		ControlDir:  t.TempDir(),
	}
	if err := b.Stop(context.Background(), inst); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !hasCall(f, "systemctl", "stop") {
		t.Fatalf("the pending unit was not stopped: %#v", f.recorded())
	}
}
