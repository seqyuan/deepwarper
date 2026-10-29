package agent

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/rendezvous"
)

func TestParseReadyToken(t *testing.T) {
	token, err := parseReadyToken("dsh web: http://alice.localhost:7658/?token=abc_-123")
	if err != nil {
		t.Fatal(err)
	}
	if token != "abc_-123" {
		t.Fatalf("got %q", token)
	}
	if _, err := parseReadyToken("dsh web: http://alice.localhost:7658/"); err == nil {
		t.Fatal("expected missing token error")
	}
}

func TestRedactingWriterSuppressesToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	w := &redactingWriter{w: f}
	if _, err := w.Write([]byte("dsh web: http://x/?token=secret\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("token leaked into log: %s", data)
	}
	if !strings.Contains(string(data), "suppressed") {
		t.Fatalf("expected suppression notice: %s", data)
	}
}

func slowDsh(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dsh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func testSpec(t *testing.T, dsh, readyTimeout string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"home", "ws", "control", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec := Spec{
		InstanceID:   "alice-default",
		Incarnation:  "inc1",
		UnitName:     "dw-alice-3f2a1b9c.service",
		Profile:      "default",
		DshBin:       dsh,
		Home:         filepath.Join(root, "home"),
		Workspace:    filepath.Join(root, "ws"),
		ControlDir:   filepath.Join(root, "control"),
		LogPath:      filepath.Join(root, "logs", "alice.log"),
		PublicURL:    "http://alice.localhost/",
		TrustedHost:  "alice.localhost",
		Port:         23456,
		ReadyTimeout: readyTimeout,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "spec.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, spec.ControlDir
}

func readControl(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestReadyTimeoutFromSpec(t *testing.T) {
	specPath, controlDir := testSpec(t, slowDsh(t), "100ms")
	start := time.Now()
	code := Run(t.Context(), specPath)
	elapsed := time.Since(start)
	if code != ExitReadyTimeout {
		t.Fatalf("exit code: %d want %d", code, ExitReadyTimeout)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("ready timeout not honoured: %s", elapsed)
	}
	if got := readControl(t, controlDir, "state"); got != "failed" {
		t.Fatalf("state: %q", got)
	}
	if got := readControl(t, controlDir, "exit_code"); got != "6" {
		t.Fatalf("exit_code: %q", got)
	}
	if got := readControl(t, controlDir, "pid_start"); got == "" {
		t.Fatal("pid_start was not published")
	}
}

func TestSIGTERMDuringReadyWaitStopsDsh(t *testing.T) {
	specPath, controlDir := testSpec(t, slowDsh(t), "20s")

	// Register a test handler so a signal that arrives before the agent
	// installs its own handler cannot terminate the test binary.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	done := make(chan int, 1)
	go func() { done <- Run(t.Context(), specPath) }()

	if !waitFor(t, 10*time.Second, func() bool {
		return readControl(t, controlDir, "state") == "starting" && readControl(t, controlDir, "pid") != ""
	}) {
		t.Fatal("agent never reached starting")
	}
	// Give the agent time to install its SIGTERM handler.
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-done:
		if code != 143 {
			t.Fatalf("exit code: %d want 143", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("agent did not return after SIGTERM")
	}
	if got := readControl(t, controlDir, "state"); got != "failed" {
		t.Fatalf("state: %q", got)
	}
	if got := readControl(t, controlDir, "exit_code"); got != "143" {
		t.Fatalf("exit_code: %q", got)
	}
}

// TestPortZeroPublishesChosenPort: port 0 asks this process to pick a free
// loopback port and publish it in ready.json.
func TestPortZeroPublishesChosenPort(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"home", "ws", "control", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dsh := filepath.Join(root, "dsh")
	script := "#!/bin/sh\necho \"dsh web: http://127.0.0.1:1/?token=abc123\"\nsleep 1\n"
	if err := os.WriteFile(dsh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controlDir := filepath.Join(root, "control")
	spec := Spec{
		InstanceID:  "alice-default",
		Incarnation: "inc1",
		Profile:     "default",
		DshBin:      dsh,
		Home:        filepath.Join(root, "home"),
		Workspace:   filepath.Join(root, "ws"),
		ControlDir:  controlDir,
		LogPath:     filepath.Join(root, "logs", "alice.log"),
		PublicURL:   "http://alice.localhost/",
		TrustedHost: "alice.localhost",
		Port:        0,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(root, "spec.json")
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(t.Context(), specPath); code != 0 {
		t.Fatalf("exit code: %d", code)
	}
	ready, err := rendezvous.ReadReady(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Port <= 0 || ready.Port > 65535 {
		t.Fatalf("ready.json must publish the chosen port: %+v", ready)
	}
	if ready.Host != "127.0.0.1" {
		t.Fatalf("host: %q", ready.Host)
	}
}

func TestPrefixCommand(t *testing.T) {
	spec := Spec{
		DshBin:      "/opt/dw/bin/dsh",
		Profile:     "default",
		Port:        23456,
		PublicURL:   "http://alice.localhost/",
		TrustedHost: "alice.localhost",
	}
	looked := false
	argv, err := prefixCommand(spec, func(string) (string, error) {
		looked = true
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if looked {
		t.Fatal("lookPath called without bwrap argv")
	}
	if argv[0] != spec.DshBin || !strings.Contains(strings.Join(argv, " "), "--profile default") {
		t.Fatalf("plain argv: %#v", argv)
	}

	spec.BwrapArgv = []string{"bwrap", "--bind", "/var/ws", "/workspace", "--", "ignored"}
	_, err = prefixCommand(spec, func(string) (string, error) { return "", os.ErrNotExist })
	if err == nil || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("missing bwrap: %v", err)
	}
	argv, err = prefixCommand(spec, func(name string) (string, error) {
		if name == "bwrap" {
			return "/usr/bin/bwrap", nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/bwrap" {
		t.Fatalf("argv prefix: %#v", argv)
	}
	if !strings.Contains(strings.Join(argv, " "), "--bind /var/ws /workspace -- "+spec.DshBin) {
		t.Fatalf("prefixed argv: %#v", argv)
	}
	if strings.Contains(strings.Join(argv, " "), "ignored") {
		t.Fatalf("stale command kept: %#v", argv)
	}

	spec.PatchPath = "/data/dw/control/alice/runtime.patch.yml"
	spec.BwrapArgv = []string{"bwrap", "--dev-bind", "/dev/fuse", "/dev/fuse", "--", "/bin/sh", "/dw/control/dw-wrap.sh"}
	argv, err = prefixCommand(spec, func(name string) (string, error) {
		if name == "bwrap" {
			return "/usr/bin/bwrap", nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(argv, " ")
	if !strings.Contains(text, "/bin/sh /dw/control/dw-wrap.sh "+spec.DshBin) {
		t.Fatalf("wrapper must run before dsh: %s", text)
	}
	if !strings.Contains(text, "--patch /dw/control/runtime.patch.yml") {
		t.Fatalf("patch path must be inside the sandbox: %s", text)
	}
	if strings.Contains(text, spec.PatchPath) {
		t.Fatalf("host patch path leaked into the sandbox command: %s", text)
	}
}

func TestSandboxEnvDropsHostSecrets(t *testing.T) {
	got := sandboxEnv(
		[]string{"PATH=/usr/bin", "HOME=/root", "DW_PASSWORD=s3cret", "LANG=C.UTF-8", "RANDOM=1"},
		[]string{"PATH=/opt/dw/bin:/opt/moniforge/bin", "HOME=/home/dsh", "DSH_HOME=/home/dsh"},
	)
	text := strings.Join(got, "\n")
	if strings.Contains(text, "DW_PASSWORD") || strings.Contains(text, "s3cret") || strings.Contains(text, "RANDOM=") {
		t.Fatalf("host env leaked: %#v", got)
	}
	for _, want := range []string{"PATH=/opt/dw/bin:/opt/moniforge/bin", "HOME=/home/dsh", "DSH_HOME=/home/dsh", "LANG=C.UTF-8"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %#v", want, got)
		}
	}
}

func TestResolveRcloneUsesLookPath(t *testing.T) {
	argv, err := resolveRcloneArgv([]string{"rclone", "mount", "r:b/p", "/mnt"}, func(name string) (string, error) {
		if name == "rclone" {
			return "/usr/bin/rclone", nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/rclone" {
		t.Fatalf("argv: %#v", argv)
	}
}

func TestRunRewritesWrapperAfterClear(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"home", "ws", "control", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	control := filepath.Join(root, "control")
	wrapPath := filepath.Join(control, "dw-wrap.sh")
	if err := os.WriteFile(wrapPath, []byte("stale wrapper\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rendezvous.Clear(control); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrapPath); !os.IsNotExist(err) {
		t.Fatal("clear must remove dw-wrap.sh")
	}
	const marker = "FRESH-WRAPPER"
	dsh := filepath.Join(root, "dsh")
	if err := os.WriteFile(dsh, []byte("#!/bin/sh\necho \"dsh web: http://127.0.0.1:1/?token=abc123\"\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	bwrap := filepath.Join(root, "bwrap")
	script := "#!/bin/sh\n" +
		"if ! grep -q " + shellQuote(marker) + " " + shellQuote(wrapPath) + "; then\n" +
		"  echo wrapper missing >&2\n  exit 1\nfi\n" +
		"exec " + shellQuote(dsh) + "\n"
	if err := os.WriteFile(bwrap, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		InstanceID:   "alice-default",
		Incarnation:  "inc1",
		Profile:      "default",
		DshBin:       dsh,
		Home:         filepath.Join(root, "home"),
		Workspace:    filepath.Join(root, "ws"),
		ControlDir:   control,
		LogPath:      filepath.Join(root, "logs", "alice.log"),
		PublicURL:    "http://alice.localhost/",
		TrustedHost:  "alice.localhost",
		Port:         23456,
		ReadyTimeout: "5s",
		BwrapArgv:    []string{bwrap, "--", "/bin/sh", "/dw/control/dw-wrap.sh"},
		Wrapper:      "#!/bin/sh\n# " + marker + "\n",
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(root, "spec.json")
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(t.Context(), specPath); code != 0 {
		t.Fatalf("exit code: %d", code)
	}
	got, err := os.ReadFile(wrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), marker) {
		t.Fatalf("wrapper was not rewritten before bwrap: %s", got)
	}
}

func TestRunUsesBwrapPrefix(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"home", "ws", "control", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "argv.txt")
	bwrap := filepath.Join(root, "bwrap")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(logPath) + "\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--\" ]; then\n    shift\n    exec \"$@\"\n  fi\n  shift\ndone\nexit 1\n"
	if err := os.WriteFile(bwrap, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	dsh := filepath.Join(root, "dsh")
	if err := os.WriteFile(dsh, []byte("#!/bin/sh\necho \"dsh web: http://127.0.0.1:1/?token=abc123\"\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		InstanceID:   "alice-default",
		Incarnation:  "inc1",
		Profile:      "default",
		DshBin:       dsh,
		Home:         filepath.Join(root, "home"),
		Workspace:    filepath.Join(root, "ws"),
		ControlDir:   filepath.Join(root, "control"),
		LogPath:      filepath.Join(root, "logs", "alice.log"),
		PublicURL:    "http://alice.localhost/",
		TrustedHost:  "alice.localhost",
		Port:         23456,
		ReadyTimeout: "5s",
		BwrapArgv:    []string{bwrap, "--bind", filepath.Join(root, "ws"), "/workspace", "--"},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(root, "spec.json")
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(t.Context(), specPath); code != 0 {
		t.Fatalf("exit code: %d", code)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	if !strings.Contains(text, "--bind") || !strings.Contains(text, dsh) {
		t.Fatalf("bwrap was not prefixed: %s", text)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestPortZeroIsPickedOnThisHost(t *testing.T) {
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 {
		t.Fatalf("port: %d", port)
	}
	// The probe must not hold the port.
	conn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("the probe leaked a listening socket: %v", err)
	}
	_ = conn.Close()
}
