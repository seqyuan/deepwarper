package instance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
)

func readyHandle() *fakeHandle {
	return &fakeHandle{
		limiter: "none",
		ready:   &rendezvous.Ready{Version: 1, State: "ready", Host: "127.0.0.1", Port: 24105},
	}
}

func TestEnsureWithoutSandboxLeavesArgvEmpty(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.starts) != 1 {
		t.Fatalf("starts: %d", len(fake.starts))
	}
	if len(fake.starts[0].BwrapArgv) != 0 || len(fake.starts[0].Rclone) != 0 {
		t.Fatalf("plain spec was wrapped: %#v", fake.starts[0])
	}
}

func TestEnsureMoniforgePlansBwrapAndInstallsConda(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	m.cfg.Runtime.Moniforge = "/opt/moniforge"
	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(fake.starts[0].BwrapArgv, "\n")
	if !strings.Contains(argv, "--ro-bind") || !strings.Contains(argv, "/opt/moniforge") {
		t.Fatalf("bwrap argv: %s", argv)
	}
	if !strings.Contains(argv, "/workspace") || !strings.Contains(argv, "/home/dsh") {
		t.Fatalf("private view missing: %s", argv)
	}
	if _, err := os.Stat(filepath.Join(inst.Home, ".condarc")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(m.cfg.Server.DataDir, "bin", "dw-env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("dw-env mode: %o", fi.Mode().Perm())
	}
	spec := fake.starts[0]
	if !strings.Contains(spec.PatchContent, "documentsDirectory: /workspace") || strings.Contains(spec.PatchContent, inst.Workspace) {
		t.Fatalf("sandbox patch must use /workspace, not the host path: %s", spec.PatchContent)
	}
	argv = strings.Join(spec.BwrapArgv, "\n")
	patch := filepath.Join(spec.ControlDir, "runtime.patch.yml")
	if !strings.Contains(argv, patch) || !strings.Contains(argv, "/dw/control/runtime.patch.yml") {
		t.Fatalf("patch file was not bound: %s", argv)
	}
	for _, a := range spec.BwrapArgv {
		if a == spec.ControlDir || strings.HasSuffix(a, "spec.json") || strings.Contains(a, "/token") {
			t.Fatalf("control directory leaked into bwrap argv: %q", a)
		}
	}
	if !strings.Contains(argv, spec.DistPath) {
		t.Fatalf("dist host path was not bound: %s", argv)
	}
	dshDir := filepath.Dir(spec.DshBin)
	if !strings.Contains(argv, dshDir) {
		t.Fatalf("dsh directory was not bound: %s", argv)
	}
	var path string
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	sep := string(os.PathListSeparator)
	wantPath := strings.Join([]string{dshDir, "/opt/moniforge/bin", "/opt/dw/env", "/usr/bin", "/bin"}, sep)
	if path != wantPath {
		t.Fatalf("PATH: %q want %q", path, wantPath)
	}
	link, err := os.Readlink(filepath.Join(inst.Workspace, "deepseek-harness", "default-workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "/workspace" {
		t.Fatalf("default-workspace -> %q, want /workspace", link)
	}
}

func TestSandboxBindsEnabledPlugin(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	m.cfg.Runtime.Moniforge = "/opt/moniforge"
	cfgDir := t.TempDir()
	users := filepath.Join(cfgDir, "users")
	if err := auth.Create(users, "alice", "pw"); err != nil {
		t.Fatal(err)
	}
	office := filepath.Join(cfgDir, "office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := "plugins:\n  - name: office\n    path: " + office + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "catalog.yaml"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := auth.Load(users, "alice")
	if err != nil {
		t.Fatal(err)
	}
	u.Plugins = []string{"office"}
	if err := auth.Save(users, u); err != nil {
		t.Fatal(err)
	}
	m.cfg.Auth.UsersDir = users
	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err != nil {
		t.Fatal(err)
	}
	bound := false
	argv := fake.starts[0].BwrapArgv
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--ro-bind" && argv[i+1] == office && argv[i+2] == office {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("plugin path was not bind-mounted: %#v", argv)
	}
}

func TestEnsureBucketMountHidesSecret(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	cfgDir := t.TempDir()
	users := filepath.Join(cfgDir, "users")
	if err := auth.Create(users, "alice", "pw"); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(cfgDir, "cred")
	const secret = "super-secret-value"
	if err := os.WriteFile(cred, []byte("access_key_id=AKIAtest\nsecret_access_key="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remotes := "remotes:\n  - name: tos1\n    provider: tos\n    endpoint: https://tos.example\n    region: cn-beijing\n    bucket: data\n    credentialFile: " + cred + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "remotes.yaml"), []byte(remotes), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := auth.Load(users, "alice")
	if err != nil {
		t.Fatal(err)
	}
	u.Mounts = []auth.Mount{{
		Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj/raw", Target: "/workspace/raw", Mode: "ro",
	}}
	if err := auth.Save(users, u); err != nil {
		t.Fatal(err)
	}
	m.cfg.Auth.UsersDir = users

	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err != nil {
		t.Fatal(err)
	}
	spec := fake.starts[0]
	joined := strings.Join(spec.BwrapArgv, "\n")
	if strings.Contains(joined, secret) || strings.Contains(joined, cred) {
		t.Fatalf("secret leaked into bwrap argv: %s", joined)
	}
	if len(spec.Rclone) != 1 {
		t.Fatalf("rclone: %#v", spec.Rclone)
	}
	rclone := strings.Join(spec.Rclone[0].Argv, " ")
	if strings.Contains(rclone, secret) || strings.Contains(rclone, cred) {
		t.Fatalf("secret leaked into rclone argv: %s", rclone)
	}
	if !strings.Contains(rclone, "--config") || !strings.Contains(rclone, "tos1:data/proj/raw") {
		t.Fatalf("rclone argv: %s", rclone)
	}
	cfgPath := filepath.Join(m.cfg.Server.DataDir, "runtime", "alice-default", "fuse", "raw.rclone.conf")
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("rclone config mode: %o", fi.Mode().Perm())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), secret) {
		t.Fatalf("isolated config missing secret: %s", data)
	}
	fusePoint := filepath.Join(m.cfg.Server.DataDir, "runtime", "alice-default", "fuse", "raw")
	bound := false
	for i := 0; i+2 < len(spec.BwrapArgv); i++ {
		if spec.BwrapArgv[i] == "--ro-bind" && spec.BwrapArgv[i+1] == fusePoint && spec.BwrapArgv[i+2] == "/workspace/raw" {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("host mountpoint was not bound: %#v", spec.BwrapArgv)
	}
	if strings.Contains(joined, cfgPath) || strings.Contains(joined, "secret_access_key") || strings.Contains(joined, cred) || strings.Contains(joined, "/dev/fuse") {
		t.Fatalf("config or fuse device leaked into bwrap: %s", joined)
	}
	if !strings.Contains(strings.Join(spec.Rclone[0].Argv, " "), "--allow-other") {
		t.Fatalf("rclone argv: %#v", spec.Rclone[0].Argv)
	}
}

func TestEnsureAppliesPluginsToHome(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	cfgDir := t.TempDir()
	users := filepath.Join(cfgDir, "users")
	if err := auth.Create(users, "alice", "pw"); err != nil {
		t.Fatal(err)
	}
	office := filepath.Join(cfgDir, "office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := "plugins:\n  - name: office\n    path: " + office + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "catalog.yaml"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := auth.Load(users, "alice")
	if err != nil {
		t.Fatal(err)
	}
	u.Plugins = []string{"office"}
	if err := auth.Save(users, u); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(m.cfg.Server.DataDir, "homes", "alice")
	profile := filepath.Join(home, "profiles", "default", "cordis.patch.yml")
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("- id: keep\n  disabled: true\n")
	if err := os.WriteFile(profile, original, 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Auth.UsersDir = users

	inst, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(inst.Home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "dw-plugin-office") || !strings.Contains(string(data), office) {
		t.Fatalf("patch: %s", data)
	}
	got, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("profile patch changed: %s", got)
	}
	if len(fake.starts[0].BwrapArgv) != 0 {
		t.Fatal("plugins alone should not force bwrap")
	}
}

func TestEnsureRemovesDisabledPluginsFromDataHome(t *testing.T) {
	fake := &fakeBackend{handle: readyHandle()}
	m := newTestManager(t, fake)
	cfgDir := t.TempDir()
	users := filepath.Join(cfgDir, "users")
	if err := auth.Create(users, "alice", "pw"); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(m.cfg.Server.DataDir, "homes", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "- id: keep\n  disabled: true\n- id: dw-plugin-office\n  plugin: /opt/dw/catalog/office\n"
	if err := os.WriteFile(filepath.Join(home, "user-cordis.patch.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Auth.UsersDir = users

	if _, err := m.Ensure(context.Background(), Request{User: "alice", Distribution: "default"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dw-plugin-office") {
		t.Fatalf("disabled plugin remained in the data dir home: %s", data)
	}
	if !strings.Contains(string(data), "keep") {
		t.Fatalf("foreign row removed: %s", data)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "homes", "alice", "user-cordis.patch.yml")); !os.IsNotExist(err) {
		t.Fatalf("plugin edit landed in configDir/homes: %v", err)
	}
}
