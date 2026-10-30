package distribution

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seqyuan/deepwarper/internal/config"
)

func TestMaterializeIsNonDestructive(t *testing.T) {
	root := t.TempDir()
	distDir := filepath.Join(root, "dist", "profile")
	if err := os.MkdirAll(distDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distDir, "package.json"), []byte(`{"name":"p"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distDir, "cordis.patch.yml"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	dist := config.DistSpec{Name: "default", Version: "1", ProfileName: "default", Path: distDir}
	if err := Materialize(home, dist); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(home, "profiles", "default", "cordis.patch.yml")
	if err := os.WriteFile(patch, []byte("# user change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(home, dist); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(patch)
	if string(data) != "# user change\n" {
		t.Fatalf("materialize overwrote user patch: %q", data)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "plugins")); err != nil {
		t.Fatal(err)
	}
	userPatch, err := os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(userPatch) != "[]\n" {
		t.Fatalf("user patch seed: %q", userPatch)
	}
}
