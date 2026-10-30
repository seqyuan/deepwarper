package cliops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/mount"
	"github.com/seqyuan/deepwarper/internal/plugin"
	"github.com/seqyuan/deepwarper/internal/state"
)

func TestDataDirFromServeConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  dataDir: dw-data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DataDirFromServeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "dw-data") {
		t.Fatalf("data dir: %q", got)
	}
	if _, err := DataDirFromServeConfig(""); err == nil {
		t.Fatal("missing serve config was accepted")
	}
}

func TestUserMountRemoteCatalogPlugin(t *testing.T) {
	dir := t.TempDir()
	if err := UserCreate(dir, "alice", "plain-secret-xyz"); err != nil {
		t.Fatal(err)
	}
	if err := UserCreate(dir, "1bad", "secret"); err == nil {
		t.Fatal("invalid user accepted")
	}
	ok, err := auth.Authenticate(filepath.Join(dir, "users"), "alice", "plain-secret-xyz")
	if err != nil || !ok {
		t.Fatalf("authenticate: %v %v", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "users", "alice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "plain-secret-xyz") {
		t.Fatal("plaintext password stored")
	}

	if err := UserSetPassword(dir, "alice", "next-secret"); err != nil {
		t.Fatal(err)
	}
	ok, err = auth.Authenticate(filepath.Join(dir, "users"), "alice", "next-secret")
	if err != nil || !ok {
		t.Fatalf("new password: %v %v", ok, err)
	}

	dataRoot := filepath.Join(dir, "srv", "data")
	workRoot := filepath.Join(dir, "srv", "work")
	mountRef := filepath.Join(dataRoot, "ref")
	for _, p := range []string{mountRef, workRoot} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	roots := "roots:\n  - id: data\n    host: " + dataRoot + "\n    allowRw: false\n  - id: work\n    host: " + workRoot + "\n    allowRw: true\n"
	if err := os.WriteFile(filepath.Join(dir, "roots.yaml"), []byte(roots), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MountAddLocal(dir, "alice", mountRef+":/data/ref:ro"); err != nil {
		t.Fatal(err)
	}
	if err := MountAddRemote(dir, "alice", "tos1", "proj/raw", "/workspace/raw", "rw"); err != nil {
		t.Fatal(err)
	}
	u, err := auth.Load(filepath.Join(dir, "users"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Mounts) != 2 {
		t.Fatalf("mounts: %+v", u.Mounts)
	}
	if u.Mounts[0].Kind != "path" || u.Mounts[0].Host != mountRef || u.Mounts[0].Target != "/data/ref" || u.Mounts[0].Mode != "ro" {
		t.Fatalf("local mount: %+v", u.Mounts[0])
	}
	if u.Mounts[1].Kind != "bucket" || u.Mounts[1].Name != "raw" || u.Mounts[1].Remote != "tos1" || u.Mounts[1].Prefix != "proj/raw" || u.Mounts[1].Target != "/workspace/raw" || u.Mounts[1].Mode != "rw" {
		t.Fatalf("remote mount: %+v", u.Mounts[1])
	}
	if err := MountAddRemote(dir, "alice", "tos1", "proj/other", "/workspace/other", "ro"); err != nil {
		t.Fatal(err)
	}
	if err := MountAddRemote(dir, "alice", "tos1", "proj/bad", "/tmp/nope", "ro"); err == nil {
		t.Fatal("bad target was saved")
	}
	u, err = auth.Load(filepath.Join(dir, "users"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Mounts) != 3 || u.Mounts[2].Name != "other" || u.Mounts[2].Prefix != "proj/other" || u.Mounts[2].Remote != "tos1" {
		t.Fatalf("second prefix: %+v", u.Mounts)
	}

	cred := filepath.Join(dir, "cred")
	if err := RemoteAdd(dir, mount.Remote{
		Name: "tos1", Provider: "tos", Endpoint: "https://tos.example", Region: "cn-beijing",
		Bucket: "data", CredentialFile: cred,
	}); err != nil {
		t.Fatal(err)
	}
	remotes, err := mount.LoadRemotes(filepath.Join(dir, "remotes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 1 || remotes[0].Provider != "tos" || remotes[0].Bucket != "data" || remotes[0].CredentialFile != cred {
		t.Fatalf("remotes: %+v", remotes)
	}
	if err := RemoteAdd(dir, mount.Remote{
		Name: "s3main", Provider: "s3", Endpoint: "https://s3.example", Region: "us-east-1",
		Bucket: "other", CredentialFile: cred,
	}); err != nil {
		t.Fatal(err)
	}
	remotes, err = mount.LoadRemotes(filepath.Join(dir, "remotes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 2 {
		t.Fatalf("remotes after second add: %+v", remotes)
	}
	if err := RemoteAdd(dir, mount.Remote{Name: "nope", Provider: "gcs"}); err == nil {
		t.Fatal("bad provider accepted")
	}

	office := filepath.Join(dir, "office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CatalogAdd(dir, "office", office); err != nil {
		t.Fatal(err)
	}
	entries, err := plugin.LoadCatalog(filepath.Join(dir, "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "office" || entries[0].Path != office {
		t.Fatalf("catalog: %+v", entries)
	}
	dataDir := filepath.Join(dir, "dw-data")
	if err := PluginEnable(dir, dataDir, "alice", "office"); err != nil {
		t.Fatal(err)
	}
	if err := PluginEnable(dir, dataDir, "alice", "office"); err != nil {
		t.Fatal(err)
	}
	u, err = auth.Load(filepath.Join(dir, "users"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Plugins) != 1 || u.Plugins[0] != "office" {
		t.Fatalf("plugins: %+v", u.Plugins)
	}
	userPlug := filepath.Join(dataDir, "homes", "alice", "plugins", "office")
	if _, err := os.Stat(userPlug); err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(dataDir, "homes", "alice", "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(patch), "dw-plugin-office") != 1 || !strings.Contains(string(patch), userPlug) || strings.Contains(string(patch), office) {
		t.Fatalf("patch must point at the user copy: %s", patch)
	}
	if _, err := os.Stat(filepath.Join(dir, "homes", "alice", "user-cordis.patch.yml")); !os.IsNotExist(err) {
		t.Fatalf("plugin was written under configDir/homes: %v", err)
	}
	if err := PluginDisable(dir, dataDir, "alice", "office"); err != nil {
		t.Fatal(err)
	}
	gone, err := os.ReadFile(filepath.Join(dataDir, "homes", "alice", "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gone), "dw-plugin-office") {
		t.Fatalf("disable left the row in the data dir home: %s", gone)
	}
	u, err = auth.Load(filepath.Join(dir, "users"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Plugins) != 0 {
		t.Fatalf("plugins after disable: %+v", u.Plugins)
	}
	if err := PluginDisable(dir, dataDir, "alice", "office"); err == nil {
		t.Fatal("second disable succeeded")
	}
	if err := UserDelete(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	// Delete tombstones the record: the file stays with no credentials and an
	// advanced generation, so delete+recreate invalidates old sessions.
	tomb, err := auth.Load(filepath.Join(dir, "users"), "alice")
	if err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if tomb.Auth.Deleted != true || tomb.Auth.PasswordHash != "" || len(tomb.Plugins) != 0 || len(tomb.Mounts) != 0 {
		t.Fatalf("tombstone record: %+v", tomb)
	}
	if tomb.Auth.SessionVersion < 2 {
		t.Fatalf("tombstone generation: %d", tomb.Auth.SessionVersion)
	}
	if ok, err := auth.Authenticate(filepath.Join(dir, "users"), "alice", "next-secret"); err != nil || ok {
		t.Fatalf("deleted user authenticated: %v %v", ok, err)
	}
	if err := UserCreate(dir, "alice", "recreated-secret"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	recreated, err := auth.Load(filepath.Join(dir, "users"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !recreated.Active() || recreated.Auth.SessionVersion < tomb.Auth.SessionVersion || recreated.Auth.SessionVersion < 2 {
		t.Fatalf("recreated record: %+v", recreated)
	}
	if ok, err := auth.Authenticate(filepath.Join(dir, "users"), "alice", "recreated-secret"); err != nil || !ok {
		t.Fatalf("recreated auth: %v %v", ok, err)
	}
}

func TestInstanceHomePrefersExistingRecord(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "dw-data")
	st, err := state.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dataDir, "homes", "alice-custom")
	inst := &model.Instance{
		ID:           "alice-default",
		User:         "alice",
		Slug:         "alice",
		Distribution: "default",
		State:        model.StateRunning,
		Home:         custom,
	}
	if err := st.Save(inst); err != nil {
		t.Fatal(err)
	}
	if got := instanceHome(dataDir, "alice"); got != custom {
		t.Fatalf("existing record home: %q", got)
	}
	if got := instanceHome(dataDir, "bob"); got != filepath.Join(dataDir, "homes", "bob") {
		t.Fatalf("default home: %q", got)
	}
	// A non-canonical identity uses the stable slug the manager uses.
	if got := instanceHome(dataDir, "Alice"); got != filepath.Join(dataDir, "homes", model.LocalSlug("Alice")) {
		t.Fatalf("stable slug home: %q", got)
	}
	// A record for a non-canonical user wins over the derived path.
	upper := &model.Instance{
		ID:           "alice-upper-default",
		User:         "Alice",
		Slug:         model.LocalSlug("Alice"),
		Distribution: "default",
		State:        model.StateStopped,
		Home:         filepath.Join(dataDir, "homes", "alice-stable"),
	}
	if err := st.Save(upper); err != nil {
		t.Fatal(err)
	}
	if got := instanceHome(dataDir, "Alice"); got != upper.Home {
		t.Fatalf("stable-slug record home: %q", got)
	}
}
