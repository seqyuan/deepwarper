package local

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

func unitTestSpec(t *testing.T) agent.Spec {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"home", "ws", "control", "logs"} {
		if err := os.MkdirAll(dir+"/"+d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return agent.Spec{
		InstanceID:  "alice-default",
		Incarnation: "inc00000001",
		UnitName:    "dw-alice-3f2a1b9c.service",
		Home:        dir + "/home",
		Workspace:   dir + "/ws",
		ControlDir:  dir + "/control",
		LogPath:     dir + "/logs/alice.log",
		CPU:         2,
		Memory:      "4Gi",
		DshBin:      "/bin/true",
	}
}

func unitBackend(f *fakeRunner) *Backend {
	return New(Options{
		DwBin:        "/path/to/dw",
		Limiter:      "systemd",
		SystemdUser:  "always",
		TasksMax:     512,
		StartTimeout: time.Second,
		StopGrace:    20 * time.Second,
		Run:          f,
	})
}

func TestStartUnitUsesUnitModeAndResetsFailedFirst(t *testing.T) {
	f := &fakeRunner{handler: lifecycleHandler("active", 4242)}
	b := unitBackend(f)
	spec := unitTestSpec(t)
	writeFile(t, spec.LogPath+".verdict", "stale")

	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.Limiter() != "systemd" {
		t.Fatalf("limiter: %q", h.Limiter())
	}
	if h.Ref() != spec.UnitName {
		t.Fatalf("ref: %q", h.Ref())
	}
	if h.PID() != 4242 {
		t.Fatalf("main pid: %d", h.PID())
	}
	if _, err := os.Stat(spec.LogPath + ".verdict"); !os.IsNotExist(err) {
		t.Fatalf("stale verdict should be removed, err=%v", err)
	}
	calls := f.recorded()
	if len(calls) < 2 {
		t.Fatalf("expected reset-failed + systemd-run, got %#v", calls)
	}
	if verb, _ := systemctlArgs(calls[0].args); verb != "reset-failed" {
		t.Fatalf("first call must be reset-failed, got %#v", calls[0])
	}
	if calls[1].name != "systemd-run" {
		t.Fatalf("second call must be systemd-run, got %#v", calls[1])
	}
	joined := strings.Join(calls[1].args, " ")
	if !strings.Contains(joined, "--unit="+spec.UnitName) {
		t.Fatalf("unit name missing: %s", joined)
	}
	if strings.Contains(joined, "--scope") {
		t.Fatalf("unit mode must not use --scope: %s", joined)
	}
}

func TestUnitStartLauncherErrorIsAppendedToLog(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemd-run" {
			return RunResult{Stderr: "systemd-run: unrecognized option '--setenv'\n", ExitCode: 1}, nil
		}
		return RunResult{}, nil
	}}
	b := unitBackend(f)
	spec := unitTestSpec(t)
	_, err := b.Start(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "unrecognized option") {
		t.Fatalf("unexpected error: %v", err)
	}
	data, readErr := os.ReadFile(spec.LogPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "launcher failed before the unit started") {
		t.Fatalf("log missing launcher banner:\n%s", data)
	}
}

func TestSystemdUserAlwaysSkipsProbe(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemd-run" {
			return RunResult{Stderr: "manager unavailable\n", ExitCode: 1}, nil
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "auto", SystemdUser: "always", Run: f})
	spec := unitTestSpec(t)
	if _, err := b.Start(context.Background(), spec); err == nil {
		t.Fatal("expected unit start failure")
	}
	if hasCall(f, "systemd-run", "--scope") {
		t.Fatal("systemdUser=always must not run the availability probe")
	}
	if !hasCall(f, "systemd-run", "--unit=") {
		t.Fatal("systemdUser=always must still attempt the unit")
	}
}

func TestSystemdUserNeverSpraysCommands(t *testing.T) {
	f := &fakeRunner{paths: map[string]string{"prlimit": "/usr/bin/prlimit"}}
	b := New(Options{DwBin: "/bin/true", Limiter: "auto", SystemdUser: "never", Run: f})
	spec := unitTestSpec(t)
	spec.Memory = ""
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.Limiter() != "prlimit" {
		t.Fatalf("limiter: %q", h.Limiter())
	}
	if len(f.recorded()) != 0 {
		t.Fatalf("systemdUser=never must not invoke systemd, got %#v", f.recorded())
	}
	_ = h.Stop(context.Background())
}

func TestAutoFallsBackToPrlimit(t *testing.T) {
	f := &fakeRunner{paths: map[string]string{"prlimit": "/usr/bin/prlimit"}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "auto", Run: f})
	res, err := b.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Limiter != "prlimit" {
		t.Fatalf("limiter: %q (%s)", res.Limiter, res.SystemdDetail)
	}
	if res.SystemdAvailable {
		t.Fatal("systemd should not be available without systemd-run")
	}
}

func TestExplicitSystemdFailsWithoutSystemd(t *testing.T) {
	f := &fakeRunner{paths: map[string]string{"prlimit": "/usr/bin/prlimit"}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", Run: f})
	if _, err := b.Resolve(context.Background()); err == nil {
		t.Fatal("explicit systemd must fail when systemd is unavailable")
	}
}

func TestStopDoesNotPreProbe(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemctl" {
			verb, _ := systemctlArgs(args)
			if verb == "show" {
				return showOutput("inactive", 0, "success", 0), nil
			}
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", StopGrace: time.Second, Run: f})
	err := b.Stop(context.Background(), &model.Instance{BackendRef: "dw-a.service", LogPath: "/tmp/a.log"})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) == 0 {
		t.Fatal("no calls recorded")
	}
	if verb, _ := systemctlArgs(calls[0].args); verb != "stop" {
		t.Fatalf("first systemctl call must be stop, got %#v", calls[0])
	}
	for _, c := range calls {
		verb, _ := systemctlArgs(c.args)
		if verb == "is-active" || verb == "list-units" {
			t.Fatalf("stop must not pre-probe: %#v", c)
		}
	}
}

func TestStopTreatsNotLoadedAsSuccess(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemctl" {
			verb, _ := systemctlArgs(args)
			switch verb {
			case "stop":
				return RunResult{Stderr: "Unit dw-a.service not loaded.\n", ExitCode: 5}, nil
			case "show":
				return RunResult{Stdout: "LoadState=not-found\nActiveState=inactive\n"}, nil
			}
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", StopGrace: time.Second, Run: f})
	if err := b.Stop(context.Background(), &model.Instance{BackendRef: "dw-a.service"}); err != nil {
		t.Fatalf("not loaded should be success: %v", err)
	}
}

func TestStopPropagatesDBusFailure(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemctl" {
			return RunResult{Stderr: "Failed to connect to bus\n", ExitCode: 1}, nil
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", StopGrace: time.Second, Run: f})
	err := b.Stop(context.Background(), &model.Instance{BackendRef: "dw-a.service"})
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("expected DBus error, got %v", err)
	}
}

func TestStopIdempotent(t *testing.T) {
	f := &fakeRunner{handler: lifecycleHandler("inactive", 0)}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", StopGrace: time.Second, Run: f})
	h := &Handle{backend: b, mode: modeUnit, unit: "dw-a.service", logPath: "/tmp/a.log", done: make(chan struct{})}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(f.recorded())
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.recorded()) != before {
		t.Fatalf("second Stop issued calls: %#v", f.recorded()[before:])
	}
	if hasCall(f, "systemctl", "SIGKILL") {
		t.Fatal("no kill expected")
	}
}

func TestStopEscalatesToKillAfterGrace(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemctl" {
			verb, _ := systemctlArgs(args)
			if verb == "show" {
				return showOutput("active", 111, "success", 0), nil
			}
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", StopGrace: 10 * time.Millisecond, Run: f})
	err := b.Stop(context.Background(), &model.Instance{BackendRef: "dw-a.service"})
	if err == nil {
		t.Fatal("expected an error when the unit never stops")
	}
	if !hasCall(f, "systemctl", "SIGKILL") {
		t.Fatalf("expected SIGKILL escalation, got %#v", f.recorded())
	}
}

func TestUnitStatusParsing(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		errOut string
		phase  runtime.Phase
		pid    int
		detail string
	}{
		{"active", "LoadState=loaded\nActiveState=active\nMainPID=99\n", "", runtime.PhaseActive, 99, ""},
		{"activating", "LoadState=loaded\nActiveState=activating\nSubState=auto-restart\n", "", runtime.PhaseActivating, 0, ""},
		{"failed oom", "LoadState=loaded\nActiveState=failed\nResult=oom-kill\nExecMainStatus=137\n", "", runtime.PhaseFailed, 0, "result=oom-kill exit=137"},
		{"not found", "LoadState=not-found\nActiveState=inactive\n", "", runtime.PhaseNotFound, 0, ""},
		{"gone stderr", "", "Unit dw-a.service not loaded.\n", runtime.PhaseNotFound, 0, "Unit dw-a.service not loaded."},
		{"unknown empty", "", "", runtime.PhaseUnknown, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := parseSystemctlShow(tc.out, tc.errOut)
			if st.Phase != tc.phase {
				t.Fatalf("phase: %q want %q", st.Phase, tc.phase)
			}
			if st.MainPID != tc.pid {
				t.Fatalf("pid: %d want %d", st.MainPID, tc.pid)
			}
			if !strings.Contains(st.Detail, tc.detail) {
				t.Fatalf("detail: %q want contains %q", st.Detail, tc.detail)
			}
		})
	}
}

func TestActivatingIsAliveButNotGone(t *testing.T) {
	p := runtime.PhaseActivating
	if !p.Alive() {
		t.Fatal("activating must be alive")
	}
	if p.Gone() {
		t.Fatal("activating must not be gone")
	}
}

func TestUnknownIsNotGone(t *testing.T) {
	if runtime.PhaseUnknown.Gone() {
		t.Fatal("unknown must never be gone")
	}
}

func TestThreeNegativeReadingsBeforeGone(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemctl" {
			return showOutput("inactive", 0, "success", 0), nil
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", Run: f})
	inst := &model.Instance{BackendRef: "dw-a.service"}
	for i := 1; i <= 2; i++ {
		obs, err := b.Probe(context.Background(), inst)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Gone() {
			t.Fatalf("reading %d must not be gone yet", i)
		}
		if obs.NegativeReadings != i {
			t.Fatalf("readings: %d want %d", obs.NegativeReadings, i)
		}
	}
	obs, err := b.Probe(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Gone() {
		t.Fatalf("third negative reading should be gone: %#v", obs)
	}
	// A live reading resets the streak.
	f.handler = lifecycleHandler("active", 7)
	if _, err := b.Probe(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	f.handler = func(name string, args []string) (RunResult, error) {
		return showOutput("inactive", 0, "success", 0), nil
	}
	obs, _ = b.Probe(context.Background(), inst)
	if obs.NegativeReadings != 1 {
		t.Fatalf("streak should reset, got %d", obs.NegativeReadings)
	}
}

func TestProbeUsesPIDPathForLegacyRecords(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		t.Fatalf("legacy pid probe must not call systemd: %s %v", name, args)
		return RunResult{}, nil
	}}
	b := New(Options{Run: f})
	inst := &model.Instance{PID: os.Getpid(), PIDStart: pidStartTime(os.Getpid())}
	obs, err := b.Probe(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Source != "pid" || !obs.Alive() {
		t.Fatalf("observation: %#v", obs)
	}
	inst.PIDStart++
	obs, _ = b.Probe(context.Background(), inst)
	if !obs.Gone() {
		t.Fatalf("reused pid should be gone: %#v", obs)
	}
}

func TestEndpointSurfacesOOMVerdict(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (RunResult, error) {
		if name == "systemctl" {
			return showOutput("failed", 0, "oom-kill", 137), nil
		}
		return RunResult{}, nil
	}}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", Run: f})
	dir := t.TempDir()
	writeFile(t, dir+"/alice.log.verdict", "KILL oom-kill\n")
	h := &Handle{
		backend:     b,
		mode:        modeUnit,
		unit:        "dw-a.service",
		controlDir:  dir,
		incarnation: "inc1",
		logPath:     dir + "/alice.log",
		done:        make(chan struct{}),
	}
	start := time.Now()
	_, err := h.Endpoint(context.Background(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "oom-kill") {
		t.Fatalf("expected oom-kill error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("endpoint took %s, expected a fast failure", elapsed)
	}
}

func TestEndpointTimesOutWhenUnitStaysActiveWithoutReady(t *testing.T) {
	f := &fakeRunner{handler: lifecycleHandler("active", 123)}
	b := New(Options{DwBin: "/path/to/dw", Limiter: "systemd", SystemdUser: "always", Run: f})
	dir := t.TempDir()
	h := &Handle{
		backend:     b,
		mode:        modeUnit,
		unit:        "dw-a.service",
		controlDir:  dir,
		incarnation: "inc1",
		logPath:     dir + "/alice.log",
		done:        make(chan struct{}),
	}
	_, err := h.Endpoint(context.Background(), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestResolveLimiterMatrix(t *testing.T) {
	cases := []struct {
		limiter string
		user    string
		sysOK   bool
		prOK    bool
		want    string
		wantErr bool
	}{
		{"auto", "auto", true, true, "systemd", false},
		{"auto", "auto", false, true, "prlimit", false},
		{"auto", "auto", false, false, "none", false},
		{"auto", "always", true, true, "systemd", false},
		{"auto", "never", true, true, "prlimit", false},
		{"systemd", "auto", true, true, "systemd", false},
		{"systemd", "never", true, true, "", true},
		{"systemd", "auto", false, true, "", true},
		{"prlimit", "auto", true, true, "prlimit", false},
		{"prlimit", "always", true, true, "", true},
		{"none", "auto", true, true, "none", false},
		{"none", "always", true, true, "", true},
		{"bogus", "auto", true, true, "", true},
	}
	for _, tc := range cases {
		got, err := ResolveLimiter(tc.limiter, tc.user, tc.sysOK, tc.prOK)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s/%s: expected error", tc.limiter, tc.user)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.limiter, tc.user, err)
		}
		if got != tc.want {
			t.Fatalf("%s/%s: got %q want %q", tc.limiter, tc.user, got, tc.want)
		}
	}
}
