package runtime_test

import (
	"testing"

	"github.com/seqyuan/deepwarper/internal/runtime"
	"github.com/seqyuan/deepwarper/internal/runtime/local"
)

var (
	_ runtime.Backend = (*local.Backend)(nil)
	_ runtime.Handle  = (*local.Handle)(nil)
)

func TestPhaseAliveVocabulary(t *testing.T) {
	alive := map[runtime.Phase]bool{
		runtime.PhaseActive:       true,
		runtime.PhaseActivating:   true,
		runtime.PhaseReloading:    true,
		runtime.PhaseDeactivating: true,
		runtime.PhaseFailed:       false,
		runtime.PhaseInactive:     false,
		runtime.PhaseNotFound:     false,
		runtime.PhaseUnknown:      false,
	}
	for phase, want := range alive {
		if got := phase.Alive(); got != want {
			t.Fatalf("%s.Alive() = %v want %v", phase, got, want)
		}
		if want && phase.Gone() {
			t.Fatalf("%s is alive and gone at once", phase)
		}
	}
	if !runtime.PhaseFailed.Gone() || !runtime.PhaseInactive.Gone() || !runtime.PhaseNotFound.Gone() {
		t.Fatal("failed, inactive, and not-found must be gone")
	}
	if runtime.PhaseUnknown.Gone() {
		t.Fatal("unknown is never gone: a backend hiccup must not orphan work")
	}
}
