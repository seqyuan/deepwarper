package state

import (
	"os"
	"path/filepath"
	"sync"
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
	if got.Revision != 1 {
		t.Fatalf("revision: %d", got.Revision)
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

func TestSaveUsesCurrentInstancePath(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := &model.Instance{User: "alice", Distribution: "default"}
	if err := s.Save(inst); err != nil {
		t.Fatal(err)
	}
	if inst.ID != model.InstanceID("alice", "default") {
		t.Fatalf("derived id: %q", inst.ID)
	}
	if inst.Revision != 1 {
		t.Fatalf("new revision: %d", inst.Revision)
	}
	if _, err := os.Stat(s.InstancePath("alice", "default")); err != nil {
		t.Fatal(err)
	}
	// Saving again bumps the revision monotonically.
	if err := s.Save(inst); err != nil {
		t.Fatal(err)
	}
	if inst.Revision != 2 {
		t.Fatalf("second revision: %d", inst.Revision)
	}
}

func TestLoadLegacyFilenameGetsRevisionOne(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &model.Instance{ID: "alice-default", User: "alice", Distribution: "default", State: model.StateRunning}
	if err := s.Save(legacy); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-revision record: no revision field on disk.
	if err := os.WriteFile(s.LegacyInstancePath("alice", "default"), []byte("id: alice-default\nuser: alice\ndistribution: default\nstate: running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "alice-default" || got.Revision != 1 {
		t.Fatalf("legacy load: %+v", got)
	}
}

func TestLoadPrefersCurrentPathAndListDedupes(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &model.Instance{ID: "alice-default", User: "alice", Distribution: "default", State: model.StateStopped}
	if err := s.Save(legacy); err != nil {
		t.Fatal(err)
	}
	current := &model.Instance{User: "alice", Distribution: "default", State: model.StateRunning}
	if err := s.Save(current); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != current.ID || got.State != model.StateRunning {
		t.Fatalf("current record must win: %+v", got)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != current.ID {
		t.Fatalf("list must dedupe: %+v", list)
	}
}

func TestLoadIgnoresMismatchedIdentity(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other := &model.Instance{ID: "alice-default", User: "bob", Distribution: "default"}
	if err := s.Save(other); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("mismatched record leaked: %+v", got)
	}
}

func TestUpdateConflictsAndCreatesOnce(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := &model.Instance{User: "alice", Distribution: "default"}
	created, err := s.Update(inst, func(cur *model.Instance) error {
		cur.State = model.StatePending
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.State != model.StatePending || created.ID == "" {
		t.Fatalf("created: %+v", created)
	}
	if _, err := s.Update(inst, func(cur *model.Instance) error { return nil }); err != ErrConflict {
		t.Fatalf("second create: got %v want ErrConflict", err)
	}
	updated, err := s.Update(created, func(cur *model.Instance) error {
		cur.State = model.StateRunning
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.State != model.StateRunning {
		t.Fatalf("updated: %+v", updated)
	}
	if _, err := s.Update(created, func(cur *model.Instance) error { return nil }); err != ErrConflict {
		t.Fatalf("stale update: got %v want ErrConflict", err)
	}
}

func TestUpdatePreservesLegacyFilename(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := &model.Instance{ID: "alice-default", User: "alice", Distribution: "default", State: model.StateRunning}
	if err := s.Save(legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(loaded, func(cur *model.Instance) error {
		cur.StopAttempts = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.LegacyInstancePath("alice", "default")); err != nil {
		t.Fatalf("legacy file must be updated in place: %v", err)
	}
	if _, err := os.Stat(s.InstancePath("alice", "default")); !os.IsNotExist(err) {
		t.Fatalf("update must not create a duplicate current file: %v", err)
	}
}

func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := &model.Instance{User: "alice", Distribution: "default"}
	if err := s.Save(inst); err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				cur, err := s.Load("alice", "default")
				if err != nil {
					errs <- err
					return
				}
				_, err = s.Update(cur, func(next *model.Instance) error {
					next.StopAttempts++
					return nil
				})
				if err == nil {
					return
				}
				if err != ErrConflict {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("update: %v", err)
	}
	got, err := s.Load("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got.StopAttempts != workers {
		t.Fatalf("lost writes: got %d want %d", got.StopAttempts, workers)
	}
}

func TestLoadByIDRejectsTraversal(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../escape", "a/b", "a\\b", "a\x00b"} {
		if _, err := s.LoadByID(id); err == nil {
			t.Fatalf("%q must be rejected", id)
		}
	}
}

func TestSaveRejectsUnsafeID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../escape", "a/b", ".", ".."} {
		inst := &model.Instance{ID: id, User: "alice", Distribution: "default"}
		if err := s.Save(inst); err == nil {
			t.Fatalf("%q must be rejected", id)
		}
	}
}

func TestLegacyInstancePathRejectsUnsafeName(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.LegacyInstancePath("a/b", "default"); got != "" {
		t.Fatalf("unsafe legacy path: %q", got)
	}
}

func TestLockDataDirExcludesSecondOwner(t *testing.T) {
	dir := t.TempDir()
	l1, err := LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockDataDir(dir); err != ErrLocked {
		t.Fatalf("second owner: got %v want ErrLocked", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "locks", "serve.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("lock file must record the owner")
	}
	l1.Release()
	l2, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l2.Release()
}
