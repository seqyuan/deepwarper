package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seqyuan/deepwarper/internal/model"
)

func TestInstanceRoundTripAndLock(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst := &model.Instance{ID: "alice-default", User: "alice", Distribution: "default", State: model.StateRunning}
	if err := s.Save(inst); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != inst.ID || got.State != model.StateRunning {
		t.Fatalf("round trip failed: %+v", got)
	}
	lockPath := filepath.Join(dir, "locks", "alice.lock")
	l1, err := Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(lockPath); err != ErrLocked {
		t.Fatalf("second lock: got %v want ErrLocked", err)
	}
	l1.Release()
	l2, err := Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
	if _, err := os.Stat(filepath.Join(dir, "instances", "alice-default.yaml")); err != nil {
		t.Fatal(err)
	}
}
