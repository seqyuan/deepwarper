package distribution

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/seqyuan/deepwarper/internal/config"
)

type ProfileState struct {
	Distribution string    `json:"distribution"`
	Version      string    `json:"version"`
	DistRoot     string    `json:"distRoot"`
	Materialized time.Time `json:"materializedAt"`
}

// Materialize prepares a user's profile directory from an immutable distribution
// profile. It never overwrites user-owned files (cordis.patch.yml after the
// first copy, .credentials.yaml, sessions/, skills/).
func Materialize(home string, dist config.DistSpec) error {
	target := filepath.Join(home, "profiles", dist.ProfileName)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	copyIfMissing := func(name string) error {
		src := filepath.Join(dist.Path, name)
		dst := filepath.Join(target, name)
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		in, err := os.Open(src)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	for _, name := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "cordis.patch.yml", "cordis.yml"} {
		if err := copyIfMissing(name); err != nil {
			return err
		}
	}

	// A profile copied from a build has node_modules; a build with no
	// out-of-tree packages may not. Link it relative to the profile so dsh's
	// module resolution sees the same tree.
	srcNM := filepath.Join(dist.Path, "node_modules")
	if _, err := os.Stat(srcNM); err == nil {
		link := filepath.Join(target, "node_modules")
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			// Existing real directory: leave it alone.
		} else {
			_ = os.Remove(link)
			if err := os.Symlink(srcNM, link); err != nil {
				return err
			}
		}
	}

	for _, dir := range []string{"sessions", "skills", "plugins"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return err
		}
	}
	userPatch := filepath.Join(home, "user-cordis.patch.yml")
	if _, err := os.Stat(userPatch); os.IsNotExist(err) {
		if err := os.WriteFile(userPatch, []byte("[]\n"), 0o600); err != nil {
			return err
		}
	}
	if seed := filepath.Join(dist.Path, "home-seed"); dirExists(seed) {
		if err := copyTreeIfMissing(seed, home); err != nil {
			return err
		}
	}
	state := ProfileState{
		Distribution: dist.Name,
		Version:      dist.Version,
		DistRoot:     dist.Path,
		Materialized: time.Now().UTC(),
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	return os.WriteFile(filepath.Join(target, ".dw-profile.json"), data, 0o600)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func copyTreeIfMissing(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

func VerifyProfile(path string) error {
	for _, name := range []string{"package.json", "cordis.patch.yml"} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			return fmt.Errorf("profile %s missing %s: %w", path, name, err)
		}
	}
	return nil
}
