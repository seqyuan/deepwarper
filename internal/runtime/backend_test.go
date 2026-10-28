package runtime_test

import (
	"testing"

	"github.com/seqyuan/deepwarper/internal/runtime"
	"github.com/seqyuan/deepwarper/internal/runtime/local"
	"github.com/seqyuan/deepwarper/internal/runtime/sge"
)

// Compile-time conformance: the manager holds every backend behind these
// interfaces, so a signature drift is a build failure rather than a runtime
// surprise.
var (
	_ runtime.Backend = (*local.Backend)(nil)
	_ runtime.Handle  = (*local.Handle)(nil)

	_ runtime.Backend     = (*sge.Backend)(nil)
	_ runtime.Policy      = (*sge.Backend)(nil)
	_ runtime.Adopter     = (*sge.Backend)(nil)
	_ runtime.Renewer     = (*sge.Backend)(nil)
	_ runtime.LeaseWarner = (*sge.Backend)(nil)
)

// TestOptionalCapabilitiesStayOptional pins the rule that a backend which does
// not implement a capability is skipped, never guessed at: local must not be
// forced to grow renewal or adoption semantics it does not have.
func TestOptionalCapabilitiesStayOptional(t *testing.T) {
	var backend runtime.Backend = local.New(local.Options{})
	if _, ok := backend.(runtime.Adopter); ok {
		t.Fatal("the local backend needs no adopte")
	}
	if _, ok := backend.(runtime.Renewer); ok {
		t.Fatal("the local backend has no scheduler lease to renew")
	}
	if _, ok := backend.(runtime.Policy); ok {
		t.Fatal("the local backend keeps the default lifecycle policy")
	}
}

func TestPhaseAliveVocabulary(t *testing.T) {
	alive := map[runtime.Phase]bool{
		runtime.PhaseActive:       true,
		runtime.PhaseActivating:   true,
		runtime.PhaseReloading:    true,
		runtime.PhaseDeactivating: true,
		runtime.PhaseQueued:       true,
		runtime.PhaseRunning:      true,
		// Suspended is alive on purpose: SGE paused the job, it did not end it.
		runtime.PhaseSuspended: true,
		runtime.PhaseFailed:    false,
		runtime.PhaseInactive:  false,
		runtime.PhaseNotFound:  false,
		runtime.PhaseGone:      false,
		runtime.PhaseUnknown:   false,
	}
	for phase, want := range alive {
		if got := phase.Alive(); got != want {
			t.Fatalf("%s.Alive() = %v want %v", phase, got, want)
		}
		if want && phase.Gone() {
			t.Fatalf("%s is alive and gone at once", phase)
		}
	}
	if !runtime.PhaseFailed.Gone() || !runtime.PhaseGone.Gone() {
		t.Fatal("failed and gone must be gone")
	}
	if runtime.PhaseUnknown.Gone() {
		t.Fatal("unknown is never gone: a backend hiccup must not orphan work")
	}
}
