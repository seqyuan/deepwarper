// Package pathutil resolves host paths physically before they are trusted.
// Every helper treats symlinks as escapes: a path is only inside a root if its
// real location is inside the root's real location.
package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the absolute, clean, symlink-resolved path. The path must
// exist.
func Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("pathutil: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("pathutil: resolve %q: %w", path, err)
	}
	return filepath.Clean(real), nil
}

// ResolveForCreate returns the path a creation would land on. The input must
// be absolute or resolvable, must not contain a ".." component, and its
// deepest existing ancestor is resolved so a symlinked parent cannot redirect
// the creation outside the caller's root.
func ResolveForCreate(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("pathutil: empty path")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return "", fmt.Errorf("pathutil: %q contains a parent reference", path)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cur := abs
	var rest []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("pathutil: cannot resolve %q", path)
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", fmt.Errorf("pathutil: resolve %q: %w", cur, err)
	}
	return filepath.Clean(filepath.Join(append([]string{real}, rest...)...)), nil
}

// Within reports whether path is root or a descendant of root. Both paths are
// cleaned; callers that need symlink safety use Resolve/ResolveUnder first.
func Within(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

// ResolveUnder resolves path and requires it to live under exactly one of the
// roots. Roots must exist and be directories; duplicate roots are ignored.
func ResolveUnder(path string, roots []string) (string, error) {
	real, err := Resolve(path)
	if err != nil {
		return "", err
	}
	if _, err := containingRoot(real, roots); err != nil {
		return "", err
	}
	return real, nil
}

// EnsureUnder creates path with mode 0700 when needed, then resolves and
// verifies containment again. It returns the real path so callers store the
// physical location rather than the requested spelling.
func EnsureUnder(path string, roots []string) (string, error) {
	planned, err := ResolveForCreate(path)
	if err != nil {
		return "", err
	}
	if _, err := containingRoot(planned, roots); err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	real, err := Resolve(path)
	if err != nil {
		return "", err
	}
	if _, err := containingRoot(real, roots); err != nil {
		return "", err
	}
	return real, nil
}

// containingRoot returns the single root that contains realPath.
func containingRoot(realPath string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("pathutil: %s has no allowed roots", realPath)
	}
	seen := map[string]bool{}
	matched := ""
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		realRoot, err := Resolve(root)
		if err != nil {
			return "", fmt.Errorf("pathutil: root %q: %w", root, err)
		}
		if seen[realRoot] {
			continue
		}
		seen[realRoot] = true
		fi, err := os.Stat(realRoot)
		if err != nil {
			return "", fmt.Errorf("pathutil: root %q: %w", root, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("pathutil: root %q is not a directory", root)
		}
		if !Within(realPath, realRoot) {
			continue
		}
		if matched != "" {
			return "", fmt.Errorf("pathutil: %s matches more than one root (%s, %s)", realPath, matched, realRoot)
		}
		matched = realRoot
	}
	if matched == "" {
		return "", fmt.Errorf("pathutil: %s is outside the allowed roots", realPath)
	}
	return matched, nil
}
