package model

import "testing"

// TestStateActiveIncludesSchedulerStates pins the set the manager treats as "a
// record we must keep looking after". queued and suspended must be in it: a job
// waiting for a slot or paused by the scheduler is still the user's work, and a
// record that is not active would be ignored by Reconcile forever.
func TestStateActiveIncludesSchedulerStates(t *testing.T) {
	active := []State{StatePending, StateStarting, StateQueued, StateSuspended, StateRunning, StateStopping}
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

func TestInstanceSchedulerFieldsRoundTrip(t *testing.T) {
	inst := Instance{
		BackendRef:     "12345",
		Backend:        "sge",
		Node:           "node001",
		JobPort:        41002,
		RemoteEndpoint: "node001:41002",
	}
	// The field names are the record's schema: renaming them silently drops data
	// for running instances.
	if inst.BackendRef != "12345" || inst.RemoteEndpoint != "node001:41002" || inst.JobPort != 41002 {
		t.Fatalf("record: %+v", inst)
	}
}
