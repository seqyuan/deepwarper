package model

import "testing"

// TestStateActive pins the records Reconcile keeps looking after.
func TestStateActive(t *testing.T) {
	active := []State{StatePending, StateStarting, StateRunning, StateStopping}
	for _, s := range active {
		if !s.Active() {
			t.Fatalf("%s must be active", s)
		}
		if s.Terminal() {
			t.Fatalf("%s must not be terminal", s)
		}
	}
	for _, s := range []State{StateStopped, StateFailed} {
		if s.Active() {
			t.Fatalf("%s must not be active", s)
		}
		if !s.Terminal() {
			t.Fatalf("%s must be terminal", s)
		}
	}
}

func TestInstanceRecordFields(t *testing.T) {
	inst := Instance{
		BackendRef: "dw-alice.service",
		Backend:    "local",
		Node:       "host",
	}
	if inst.BackendRef != "dw-alice.service" || inst.Backend != "local" || inst.Node != "host" {
		t.Fatalf("record: %+v", inst)
	}
}
