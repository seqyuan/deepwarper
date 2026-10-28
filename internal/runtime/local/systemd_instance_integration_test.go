package local

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const fakeReadyDshScript = `#!/bin/sh
echo "dsh web: http://127.0.0.1:23456/?token=super-secret-token"
while :; do sleep 1; done
`

// TestRealSystemdDwInstance verifies the unit's main process is the real
// `dw instance` agent, so dsh's token-bearing stdout stays behind the agent
// redactor. Gated: needs systemd and DW_TEST_SYSTEMD_DW=<path to dw>.
func TestRealSystemdDwInstance(t *testing.T) {
	if os.Getenv("DW_TEST_SYSTEMD") == "" {
		t.Skip("set DW_TEST_SYSTEMD=1 to exercise a real systemd user unit")
	}
	dwBin := os.Getenv("DW_TEST_SYSTEMD_DW")
	if dwBin == "" {
		t.Skip("set DW_TEST_SYSTEMD_DW to the built dw binary")
	}
	dsh := t.TempDir() + "/dsh"
	if err := os.WriteFile(dsh, []byte(fakeReadyDshScript), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := fallbackSpec(t)
	spec.DshBin = dsh
	spec.UnitName = "dw-instance-it-" + itoa(os.Getpid()) + ".service"
	opts := Options{
		DwBin:        dwBin,
		Limiter:      "systemd",
		SystemdUser:  "always",
		TasksMax:     512,
		StartTimeout: 10 * time.Second,
		StopGrace:    10 * time.Second,
		UnitStdio:    "log",
	}
	b := New(opts)
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = b.stopUnit(context.Background(), spec.UnitName, 5*time.Second) })
	ready, err := h.Endpoint(context.Background(), 20*time.Second)
	if err != nil {
		data, _ := os.ReadFile(spec.LogPath)
		t.Fatalf("endpoint: %v\nlog:\n%s", err, data)
	}
	if ready.Port != spec.Port {
		t.Fatalf("ready port: %d", ready.Port)
	}
	token, err := os.ReadFile(spec.ControlDir + "/token")
	if err != nil || strings.TrimSpace(string(token)) != "super-secret-token" {
		t.Fatalf("token file: %q err=%v", token, err)
	}
	data, err := os.ReadFile(spec.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super-secret-token") || strings.Contains(string(data), "?token=") {
		t.Fatalf("token leaked into the log:\n%s", data)
	}
	if !strings.Contains(string(data), "startup line suppressed") {
		t.Fatalf("expected suppression notice:\n%s", data)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
