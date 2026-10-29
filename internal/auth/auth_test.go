package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsValidUsername(t *testing.T) {
	ok := []string{"a", "alice", "a1", "a-b", "a" + strings.Repeat("b", 31)}
	bad := []string{"", "1abc", "Alice", "_x", "a_b", "a/b", "a.b", "has space", "a" + strings.Repeat("b", 32), "..", "alice/../bob"}
	for _, name := range ok {
		if !IsValidUsername(name) {
			t.Errorf("expected valid: %q", name)
		}
	}
	for _, name := range bad {
		if IsValidUsername(name) {
			t.Errorf("expected invalid: %q", name)
		}
	}
}

func TestCreateAuthenticateAndFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "users")
	const password = "plain-secret-xyz"
	if err := Create(dir, "alice", password); err != nil {
		t.Fatal(err)
	}
	if err := Create(dir, "alice", "other"); err == nil {
		t.Fatal("duplicate user was accepted")
	}
	if err := Create(dir, "1bad", password); err == nil {
		t.Fatal("invalid username was accepted")
	}
	if err := Create(dir, "bob", ""); err == nil {
		t.Fatal("empty password was accepted")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("users dir mode: %o", info.Mode().Perm())
	}
	path := filepath.Join(dir, "alice.yaml")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("user file mode: %o", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), password) {
		t.Fatalf("plaintext password written to %s", path)
	}
	if !strings.Contains(string(data), "password_hash:") {
		t.Fatalf("missing password hash field: %s", data)
	}

	ok, err := Authenticate(dir, "alice", password)
	if err != nil || !ok {
		t.Fatalf("authenticate: %v %v", ok, err)
	}
	ok, err = Authenticate(dir, "alice", "wrong-password")
	if err != nil || ok {
		t.Fatalf("bad password: %v %v", ok, err)
	}
	ok, err = Authenticate(dir, "nobody", password)
	if err != nil || ok {
		t.Fatalf("unknown user: %v %v", ok, err)
	}
}

func TestSetPasswordAndDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "users")
	if err := Create(dir, "alice", "first-secret"); err != nil {
		t.Fatal(err)
	}
	if err := SetPassword(dir, "alice", "second-secret"); err != nil {
		t.Fatal(err)
	}
	ok, err := Authenticate(dir, "alice", "first-secret")
	if err != nil || ok {
		t.Fatalf("old password still works: %v %v", ok, err)
	}
	ok, err = Authenticate(dir, "alice", "second-secret")
	if err != nil || !ok {
		t.Fatalf("new password: %v %v", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "alice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "second-secret") || strings.Contains(string(data), "first-secret") {
		t.Fatal("plaintext password present after SetPassword")
	}
	fi, err := os.Stat(filepath.Join(dir, "alice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after SetPassword: %o", fi.Mode().Perm())
	}
	if err := Delete(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alice.yaml")); !os.IsNotExist(err) {
		t.Fatalf("user file still present: %v", err)
	}
	ok, err = Authenticate(dir, "alice", "second-secret")
	if err != nil || ok {
		t.Fatalf("deleted user authenticated: %v %v", ok, err)
	}
	if err := Delete(dir, "alice"); err == nil {
		t.Fatal("delete of missing user succeeded")
	}
}

func TestLoadSaveMountsAndPlugins(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "users")
	if err := Create(dir, "alice", "plain-secret-xyz"); err != nil {
		t.Fatal(err)
	}
	u, err := Load(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "alice" || u.Auth.PasswordHash == "" {
		t.Fatalf("loaded user: %+v", u)
	}
	u.Mounts = []Mount{{
		Name:   "ref",
		Kind:   "path",
		Host:   "/srv/data/ref",
		Target: "/data/ref",
		Mode:   "ro",
	}}
	u.Plugins = []string{"office"}
	if err := Save(dir, u); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Target != "/data/ref" || got.Mounts[0].Mode != "ro" {
		t.Fatalf("mounts: %+v", got.Mounts)
	}
	if len(got.Plugins) != 1 || got.Plugins[0] != "office" {
		t.Fatalf("plugins: %+v", got.Plugins)
	}
	if got.Auth.PasswordHash != u.Auth.PasswordHash {
		t.Fatal("save dropped the password hash")
	}
	data, err := os.ReadFile(filepath.Join(dir, "alice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "plain-secret-xyz") {
		t.Fatal("plaintext password present after Save")
	}
}
