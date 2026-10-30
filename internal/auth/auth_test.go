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
	// Deletion tombstones the record so the advanced generation survives.
	tomb, err := Load(dir, "alice")
	if err != nil {
		t.Fatalf("tombstone load: %v", err)
	}
	if tomb.Auth.Deleted != true || tomb.Auth.PasswordHash != "" || len(tomb.Mounts) != 0 || len(tomb.Plugins) != 0 {
		t.Fatalf("tombstone: %+v", tomb)
	}
	if tomb.Auth.SessionVersion < 3 {
		t.Fatalf("tombstone generation did not advance: %d", tomb.Auth.SessionVersion)
	}
	ok, err = Authenticate(dir, "alice", "second-secret")
	if err != nil || ok {
		t.Fatalf("deleted user authenticated: %v %v", ok, err)
	}
	if err := SetPassword(dir, "alice", "third-secret"); err == nil {
		t.Fatal("SetPassword accepted a deleted user")
	}
	if err := Delete(dir, "alice"); err == nil {
		t.Fatal("second delete succeeded")
	}
	if err := Delete(dir, "nobody"); err == nil {
		t.Fatal("delete of missing user succeeded")
	}
}

// TestDeleteRecreateKeepsGeneration covers the delete/recreate cycle: the new
// account must not reuse the generation of the deleted one, and old sessions
// must stay invalid.
func TestDeleteRecreateKeepsGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "users")
	if err := Create(dir, "alice", "first-secret"); err != nil {
		t.Fatal(err)
	}
	before, err := Load(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if before.Auth.SessionVersion != 1 {
		t.Fatalf("new user generation: %d", before.Auth.SessionVersion)
	}
	if err := Delete(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	deleted, err := Load(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(dir, "alice", "new-secret"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	after, err := Load(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Active() {
		t.Fatal("recreated user is not active")
	}
	// Recreate keeps the tombstone's generation (it must not reset to 1);
	// only that guarantees sessions from before the delete stay invalid.
	if after.Auth.SessionVersion < deleted.Auth.SessionVersion || after.Auth.SessionVersion <= before.Auth.SessionVersion {
		t.Fatalf("generation did not advance: before=%d deleted=%d after=%d",
			before.Auth.SessionVersion, deleted.Auth.SessionVersion, after.Auth.SessionVersion)
	}
	if after.Auth.PasswordHash == before.Auth.PasswordHash {
		t.Fatal("recreate kept the old password hash")
	}
	ok, err := Authenticate(dir, "alice", "first-secret")
	if err != nil || ok {
		t.Fatalf("old password after recreate: %v %v", ok, err)
	}
	ok, err = Authenticate(dir, "alice", "new-secret")
	if err != nil || !ok {
		t.Fatalf("new password after recreate: %v %v", ok, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "alice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after recreate: %o", fi.Mode().Perm())
	}
}

func TestActiveRequiresCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "users")
	if err := Create(dir, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	u, err := Load(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Active() {
		t.Fatal("a created account must be active")
	}
	u.Auth.PasswordHash = ""
	if u.Active() {
		t.Fatal("a credential-less record must not be active")
	}
	u.Auth.PasswordHash = "hash"
	u.Auth.Deleted = true
	if u.Active() {
		t.Fatal("a tombstoned record must not be active")
	}
	var nilUser *User
	if nilUser.Active() {
		t.Fatal("a nil user must not be active")
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
