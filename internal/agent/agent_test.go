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

// TestPortZeroPublishesChosenPort is the SGE contract: the control plane sends
// port 0 because only the job host knows which loopback port is free, and the
// chosen port must be published in ready.json.
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

// TestPortZeroIsPickedOnThisHost is the SGE contract: the control plane sends
// port 0 because only the job host knows which loopback port is free, and the
// chosen port must be published in ready.json.
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
