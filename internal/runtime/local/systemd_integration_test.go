package local

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/model"
)

// TestRealSystemdUnitLifecycle is gated because unit mode needs a running
// `systemd --user` manager. Run with DW_TEST_SYSTEMD=1.
func TestRealSystemdUnitLifecycle(t *testing.T) {
	if os.Getenv("DW_TEST_SYSTEMD") == "" {
		t.Skip("set DW_TEST_SYSTEMD=1 to exercise a real systemd user unit")
	}
	spec := fallbackSpec(t)
	dw := fakeDw(t)
	spec.UnitName = "dw-integration-" + itoa(os.Getpid()) + ".service"
	spec.Incarnation = "testinc"
	opts := Options{
		DwBin:        dw,
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
	if h.Limiter() != "systemd" {
		t.Fatalf("limiter: %q", h.Limiter())
	}
	if _, err := h.Endpoint(context.Background(), 15*time.Second); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if !h.Alive() {
		t.Fatal("unit should be active")
	}

	// Simulate a control-plane restart: a new backend has no handle, only the
	// persisted unit name.
	b2 := New(opts)
	inst := &model.Instance{BackendRef: spec.UnitName, LogPath: spec.LogPath}
	obs, err := b2.Probe(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Alive() || obs.Ref != spec.UnitName {
		t.Fatalf("adopted observation: %#v", obs)
	}
	if err := b2.Stop(context.Background(), inst); err != nil {
		t.Fatalf("stop by unit name: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	gone := false
	for {
		obs, _ = b2.Probe(context.Background(), inst)
		if obs.Gone() {
			gone = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("unit still alive after stop: %#v", obs)
	}

	// reset-failed/stop must free the name for a second start.
	spec2 := spec
	// fakeDw always publishes incarnation "testinc".
	b3 := New(opts)
	h2, err := b3.Start(context.Background(), spec2)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	defer h2.Stop(context.Background())
	if _, err := h2.Endpoint(context.Background(), 15*time.Second); err != nil {
		t.Fatalf("second endpoint: %v", err)
	}
}
