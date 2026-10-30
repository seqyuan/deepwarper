package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func mustDir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveAndWithin(t *testing.T) {
	root := mustDir(t, filepath.Join(t.TempDir(), "root"))
	sub := mustDir(t, filepath.Join(root, "sub"))

	got, err := Resolve(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !Within(got, root) || !Within(got, got) {
		t.Fatalf("%q must be within %q", got, root)
	}
	if Within(root, sub) {
		t.Fatal("parent must not be within its child")
	}
	if Within(filepath.Join(filepath.Dir(root), "other"), root) {
		t.Fatal("sibling must not be within root")
	}
	if _, err := Resolve(filepath.Join(root, "missing")); err == nil {
		t.Fatal("Resolve must fail for a missing path")
	}
}

func TestResolveForCreate(t *testing.T) {
	root := mustDir(t, filepath.Join(t.TempDir(), "root"))
	got, err := ResolveForCreate(filepath.Join(root, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "a", "b"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := ResolveForCreate(root + "/a/../b"); err == nil {
		t.Fatal("a parent reference must be rejected")
	}
}

func TestResolveUnderRejectsSymlinkEscape(t *testing.T) {
	root := mustDir(t, filepath.Join(t.TempDir(), "root"))
	outside := mustDir(t, filepath.Join(t.TempDir(), "outside"))
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUnder(link, []string{root}); err == nil {
		t.Fatal("symlink escape must be rejected")
	}
	if _, err := EnsureUnder(filepath.Join(link, "new"), []string{root}); err == nil {
		t.Fatal("EnsureUnder must reject a symlink escape")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("directory was created outside the root: %v", err)
	}
}

func TestResolveUnderFinalSymlinkEscape(t *testing.T) {
	root := mustDir(t, filepath.Join(t.TempDir(), "root"))
	outside := mustDir(t, filepath.Join(t.TempDir(), "outside"))
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUnder(link, []string{root}); err == nil {
		t.Fatal("final component symlink escape must be rejected")
	}
}

func TestResolveUnderSymlinkedRoot(t *testing.T) {
	realRoot := mustDir(t, filepath.Join(t.TempDir(), "real"))
	parent := mustDir(t, filepath.Join(t.TempDir(), "parent"))
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	sub := mustDir(t, filepath.Join(realRoot, "sub"))
	got, err := ResolveUnder(sub, []string{alias})
	if err != nil {
		t.Fatal(err)
	}
	if got != sub {
		t.Fatalf("got %q want %q", got, sub)
	}
}

func TestEnsureUnderCreatesAndReturnsRealPath(t *testing.T) {
	root := mustDir(t, filepath.Join(t.TempDir(), "root"))
	want := filepath.Join(root, "a", "b")
	got, err := EnsureUnder(want, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	fi, err := os.Stat(got)
	if err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestResolveUnderRequiresExactlyOneRoot(t *testing.T) {
	root := mustDir(t, filepath.Join(t.TempDir(), "root"))
	child := mustDir(t, filepath.Join(root, "child"))
	sub := mustDir(t, filepath.Join(child, "sub"))
	if _, err := ResolveUnder(sub, []string{root, child}); err == nil {
		t.Fatal("nested roots must be ambiguous")
	}
	if _, err := ResolveUnder(sub, []string{child, filepath.Clean(child)}); err != nil {
		t.Fatalf("duplicate roots must be deduplicated: %v", err)
	}
	if _, err := ResolveUnder(sub, nil); err == nil {
		t.Fatal("no roots must fail closed")
	}
}

func TestResolveUnderRejectsNonDirectoryRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUnder(file, []string{file}); err == nil {
		t.Fatal("a file root must be rejected")
	}
}
