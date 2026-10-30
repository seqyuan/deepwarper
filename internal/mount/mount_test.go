package mount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/auth"
)

func baseInput(t *testing.T) Input {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "srv", "data")
	work := filepath.Join(root, "srv", "work")
	for _, d := range []string{data, work, filepath.Join(root, "homes", "alice"), filepath.Join(root, "ws", "alice")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Input{
		DSHHome:   filepath.Join(root, "homes", "alice"),
		Workspace: filepath.Join(root, "ws", "alice"),
		Roots: []Root{
			{ID: "data", Host: data, AllowRW: false},
			{ID: "work", Host: work, AllowRW: true},
		},
		FuseDir: filepath.Join(root, "fuse"),
		Command: []string{"/opt/dw/bin/dsh", "--profile", "default"},
	}
}

// rootHost returns an existing host path under the named root. Every path
// mount is resolved physically, so the fixture must exist.
func rootHost(t *testing.T, in Input, rootID, rel string) string {
	t.Helper()
	for _, r := range in.Roots {
		if r.ID != rootID {
			continue
		}
		path := filepath.Join(r.Host, rel)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Fatalf("no root %q", rootID)
	return ""
}

func hasSeq(argv []string, parts ...string) bool {
	if len(parts) == 0 {
		return false
	}
	for i := 0; i+len(parts) <= len(argv); i++ {
		ok := true
		for j, p := range parts {
			if argv[i+j] != p {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestBuildEmptyPlan(t *testing.T) {
	_, err := Build(Input{
		DSHHome:   "/var/lib/dw/homes/alice",
		Workspace: "/var/lib/dw/ws/alice",
		Command:   []string{"/opt/dw/bin/dsh"},
	})
	if err == nil || !strings.Contains(err.Error(), "mount: empty plan") {
		t.Fatalf("error: %v", err)
	}
}

func TestBuildPrivateView(t *testing.T) {
	in := baseInput(t)
	in.Moniforge = "/opt/moniforge"
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BwrapArgv[0] != "bwrap" {
		t.Fatalf("argv0: %q", plan.BwrapArgv[0])
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", "/opt/moniforge", "/opt/moniforge") {
		t.Fatalf("moniforge bind missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--bind", in.Workspace, "/workspace") {
		t.Fatalf("workspace bind missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--bind", in.DSHHome, "/home/dsh") {
		t.Fatalf("home bind missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--bind", in.DSHHome, in.DSHHome) {
		t.Fatalf("home host-path bind missing for absolute plugin paths: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--", "/opt/dw/bin/dsh", "--profile", "default") {
		t.Fatalf("command missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", "/opt/dw/bin", "/opt/dw/bin") {
		t.Fatalf("dsh directory must be bound at its host path: %#v", plan.BwrapArgv)
	}

	in.Moniforge = ""
	in.Dist = "/opt/dw/dist/default"
	plan, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", in.Dist, in.Dist) {
		t.Fatalf("dist host path bind missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", in.Dist, "/opt/dw/dist") {
		t.Fatalf("dist bind missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--bind", in.Workspace, "/workspace") || !hasSeq(plan.BwrapArgv, "--bind", in.DSHHome, "/home/dsh") {
		t.Fatalf("workspace+home missing from dist plan: %#v", plan.BwrapArgv)
	}
}

func TestBuildLocalMounts(t *testing.T) {
	in := baseInput(t)
	ref := rootHost(t, in, "data", "ref")
	in.Mounts = []auth.Mount{{
		Name: "ref", Kind: "path", Host: ref, Target: "/data/ref", Mode: "ro",
	}}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", ref, "/data/ref") {
		t.Fatalf("ro bind missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--dir", "/data") {
		t.Fatalf("path mount under /data must create /data: %#v", plan.BwrapArgv)
	}

	proj := rootHost(t, in, "work", "proj")
	in.Mounts = []auth.Mount{{
		Name: "proj", Kind: "path", Host: proj, Target: "/workspace/proj", Mode: "rw",
	}}
	plan, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--bind", proj, "/workspace/proj") {
		t.Fatalf("rw bind missing: %#v", plan.BwrapArgv)
	}
}

func TestBuildRejectsBadLocalMounts(t *testing.T) {
	fixture := baseInput(t)
	roRef := rootHost(t, fixture, "data", "ref")
	rwProj := rootHost(t, fixture, "work", "proj")
	cases := []auth.Mount{
		{Name: "x", Kind: "path", Host: filepath.Join(filepath.Dir(filepath.Dir(roRef)), "data", "..", "etc"), Target: "/data/x", Mode: "ro"},
		{Name: "x", Kind: "path", Host: "/etc/passwd", Target: "/data/x", Mode: "ro"},
		{Name: "x", Kind: "path", Host: roRef, Target: "/workspace/proj", Mode: "rw"},
		{Name: "x", Kind: "path", Host: rwProj, Target: "/data/proj", Mode: "rw"},
		{Name: "x", Kind: "path", Host: rwProj, Target: "/data/a/b", Mode: "ro"},
		{Name: "x", Kind: "path", Host: rwProj, Target: "/workspace/a/b", Mode: "ro"},
		{Name: "x", Kind: "path", Host: rwProj, Target: "/workspace", Mode: "ro"},
		{Name: "x", Kind: "path", Host: rwProj, Target: "/tmp/x", Mode: "ro"},
		{Name: "x", Kind: "path", Host: "", Target: "/data/x", Mode: "ro"},
	}
	for _, m := range cases {
		in := baseInput(t)
		in.Mounts = []auth.Mount{m}
		if _, err := Build(in); err == nil {
			t.Fatalf("accepted mount %+v", m)
		}
	}

	in := baseInput(t)
	dataRoot := in.Roots[0].Host
	in.Roots = []Root{
		{ID: "wide", Host: filepath.Dir(filepath.Dir(dataRoot)), AllowRW: true},
		{ID: "data", Host: dataRoot, AllowRW: false},
	}
	ref := rootHost(t, in, "data", "ref")
	in.Mounts = []auth.Mount{{
		Name: "x", Kind: "path", Host: ref, Target: "/data/ref", Mode: "ro",
	}}
	if _, err := Build(in); err == nil {
		t.Fatal("path under two roots was accepted")
	}
}

func TestBuildAcceptsCleanedPathInsideRoot(t *testing.T) {
	in := baseInput(t)
	ref := rootHost(t, in, "data", "ref")
	for _, host := range []string{filepath.Join(in.Roots[0].Host, ".", "ref"), in.Roots[0].Host + "//ref"} {
		in.Mounts = []auth.Mount{{
			Name: "ref", Kind: "path", Host: host, Target: "/data/ref", Mode: "ro",
		}}
		plan, err := Build(in)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if !hasSeq(plan.BwrapArgv, "--ro-bind", ref, "/data/ref") {
			t.Fatalf("%s cleaned bind missing: %#v", host, plan.BwrapArgv)
		}
	}
}

func TestBuildHostBindsComeFromInput(t *testing.T) {
	in := baseInput(t)
	in.Moniforge = "/opt/moniforge"
	in.HostBinds = []string{"/no/such/host-lib"}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", "/no/such/host-lib", "/no/such/host-lib") {
		t.Fatalf("caller host bind missing: %#v", plan.BwrapArgv)
	}
	for _, p := range []string{"/usr", "/lib64", "/etc/resolv.conf"} {
		if hasSeq(plan.BwrapArgv, "--ro-bind", p, p) {
			t.Fatalf("Build stat'd a host path that was not requested: %s in %#v", p, plan.BwrapArgv)
		}
	}
}

func TestBuildBucketMount(t *testing.T) {
	cred := filepath.Join(t.TempDir(), "cred")
	const secret = "super-secret-value"
	if err := os.WriteFile(cred, []byte("access_key_id=AKIAtest\nsecret_access_key="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := baseInput(t)
	in.Bwrap = "bubblewrap"
	in.RcloneBin = "rclone-bin"
	in.Remotes = []Remote{{
		Name: "tos1", Provider: "tos", Endpoint: "https://tos.example", Region: "cn-beijing",
		Bucket: "data", CredentialFile: cred,
	}}
	in.PatchFile = "/var/lib/dw/control/alice/runtime.patch.yml"
	in.Mounts = []auth.Mount{{
		Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj/raw", Target: "/workspace/raw", Mode: "ro",
	}}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BwrapArgv[0] != "bubblewrap" {
		t.Fatalf("bwrap bin: %q", plan.BwrapArgv[0])
	}
	joined := strings.Join(plan.BwrapArgv, "\n")
	if strings.Contains(joined, secret) || strings.Contains(joined, cred) {
		t.Fatalf("secret or credential path leaked into bwrap argv: %s", joined)
	}
	if len(plan.Rclone) != 1 {
		t.Fatalf("rclone mounts: %#v", plan.Rclone)
	}
	rc := plan.Rclone[0]
	if rc.Name != "raw" {
		t.Fatalf("name: %q", rc.Name)
	}
	hostPoint := filepath.Join(in.FuseDir, "raw")
	hostCfg := ConfigPath(in.FuseDir, "raw")
	if rc.MountPoint != hostPoint {
		t.Fatalf("mount point: %q", rc.MountPoint)
	}
	argv := strings.Join(rc.Argv, " ")
	if rc.Argv[0] != "rclone-bin" {
		t.Fatalf("rclone bin: %#v", rc.Argv)
	}
	for _, want := range []string{"mount", "tos1:data/proj/raw", hostPoint, "--read-only", "--config", hostCfg, "--allow-other"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("rclone argv missing %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, secret) || strings.Contains(argv, "secret_access_key") {
		t.Fatalf("secret leaked into rclone argv: %s", argv)
	}
	if strings.Contains(joined, secret) || strings.Contains(joined, "secret_access_key") || strings.Contains(joined, cred) || strings.Contains(joined, hostCfg) || strings.Contains(joined, "/dev/fuse") {
		t.Fatalf("secret, credential, config, or fuse device leaked into bwrap argv: %s", joined)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", hostPoint, "/workspace/raw") {
		t.Fatalf("host mountpoint was not bound onto the target: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--unshare-user") || !hasSeq(plan.BwrapArgv, "--unshare-pid") || !hasSeq(plan.BwrapArgv, "--proc", "/proc") {
		t.Fatalf("pid and user namespaces missing: %#v", plan.BwrapArgv)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", in.PatchFile, "/dw/control/runtime.patch.yml") {
		t.Fatalf("patch file bind missing: %#v", plan.BwrapArgv)
	}
	for _, a := range plan.BwrapArgv {
		if a == "/var/lib/dw/control/alice" || strings.HasSuffix(a, "spec.json") || strings.Contains(a, "token") {
			t.Fatalf("control file leaked into bwrap argv: %q in %#v", a, plan.BwrapArgv)
		}
	}

	in.Mounts[0].Mode = "rw"
	plan, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(plan.Rclone[0].Argv, " "), "--read-only") {
		t.Fatalf("rw rclone argv: %#v", plan.Rclone[0].Argv)
	}
	if !hasSeq(plan.BwrapArgv, "--bind", hostPoint, "/workspace/raw") {
		t.Fatalf("rw host mount missing: %#v", plan.BwrapArgv)
	}
}

func TestBuildRejectsBadBucketMounts(t *testing.T) {
	in := baseInput(t)
	in.Remotes = []Remote{{Name: "tos1", Provider: "s3", Endpoint: "https://s3.example", Region: "us-east-1", Bucket: "data", CredentialFile: "/tmp/cred"}}
	bad := []auth.Mount{
		{Name: "raw", Kind: "bucket", Remote: "missing", Prefix: "proj", Target: "/workspace/raw", Mode: "ro"},
		{Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "", Target: "/workspace/raw", Mode: "ro"},
		{Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "/proj", Target: "/workspace/raw", Mode: "ro"},
		{Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj/../x", Target: "/workspace/raw", Mode: "ro"},
		{Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj", Target: "/data/raw", Mode: "rw"},
		{Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj", Target: "/workspace", Mode: "rw"},
		{Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj", Target: "/etc/raw", Mode: "ro"},
	}
	for _, m := range bad {
		in.Mounts = []auth.Mount{m}
		if _, err := Build(in); err == nil {
			t.Fatalf("accepted bucket mount %+v", m)
		}
	}
}

func TestDataBucketCreatesDirAndFuseWait(t *testing.T) {
	in := baseInput(t)
	in.PatchFile = "/var/lib/dw/control/runtime.patch.yml"
	in.Moniforge = "/opt/moniforge"
	in.BinDir = "/var/dw/bin"
	in.RcloneBin = "/usr/local/bin/rclone"
	in.Remotes = []Remote{{
		Name: "tos1", Provider: "s3", Endpoint: "https://s3.example", Region: "us-east-1",
		Bucket: "data", CredentialFile: "/tmp/cred",
	}}
	in.Mounts = []auth.Mount{{
		Name: "raw", Kind: "bucket", Remote: "tos1", Prefix: "proj/raw", Target: "/data/raw", Mode: "ro",
	}}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--dir", "/data") {
		t.Fatalf("missing /data directory: %#v", plan.BwrapArgv)
	}
	hostPoint := filepath.Join(in.FuseDir, "raw")
	hostCfg := ConfigPath(in.FuseDir, "raw")
	if plan.Rclone[0].Argv[0] != "/usr/local/bin/rclone" {
		t.Fatalf("rclone bin: %#v", plan.Rclone[0].Argv)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", hostPoint, "/data/raw") {
		t.Fatalf("host mount bind: %#v", plan.BwrapArgv)
	}
	joined := strings.Join(plan.BwrapArgv, "\n")
	if strings.Contains(joined, hostCfg) || strings.Contains(joined, "secret_access_key") || strings.Contains(joined, "/tmp/cred") {
		t.Fatalf("config or secret in bwrap argv: %s", joined)
	}
	if err := EnsureMountpoint(hostPoint); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(hostPoint); err != nil || !fi.IsDir() {
		t.Fatalf("mountpoint: %v", err)
	}
	fuseLine := "36 35 0:41 / " + hostPoint + " rw,nosuid,nodev - fuse.rclone rclone rw\n"
	plain := "37 35 8:1 /srv /workspace rw - ext4 /dev/sda1 rw\n"
	if !FuseReady(fuseLine+plain, hostPoint) {
		t.Fatal("fuse mount not recognized")
	}
	if FuseReady(plain, "/workspace") {
		t.Fatal("workspace bind counted as a fuse mount")
	}
	if RcloneReadyTimeout < 30*time.Second {
		t.Fatalf("ready timeout: %s", RcloneReadyTimeout)
	}
	if err := WaitFuse(hostPoint, time.Millisecond, func() (string, error) { return plain, nil }, func(time.Duration) {}); err == nil {
		t.Fatal("wait succeeded without a fuse mount")
	}
	sep := string(os.PathListSeparator)
	var path string
	for _, kv := range plan.Env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if !strings.HasPrefix(path, "/opt/dw/bin"+sep) {
		t.Fatalf("dsh dir must stay first: %q", path)
	}
	if !strings.Contains(path, sep+"/opt/moniforge/bin"+sep) {
		t.Fatalf("moniforge bin missing after dsh: %q", path)
	}
	wantTail := "/usr/bin" + sep + "/bin"
	if !strings.HasSuffix(path, wantTail) {
		t.Fatalf("PATH must end with %s: %q", wantTail, path)
	}
}

func TestTargetSegmentCharset(t *testing.T) {
	remote := []Remote{{
		Name: "tos1", Provider: "s3", Endpoint: "https://s3.example", Region: "us-east-1",
		Bucket: "data", CredentialFile: "/tmp/cred",
	}}
	for _, seg := range []string{"raw"} {
		in := baseInput(t)
		in.Mounts = []auth.Mount{{
			Name: seg, Kind: "path", Host: rootHost(t, in, "data", "ref"), Target: "/data/" + seg, Mode: "ro",
		}}
		if _, err := Build(in); err != nil {
			t.Fatalf("path target %s: %v", seg, err)
		}
		in = baseInput(t)
		in.Remotes = remote
		in.Mounts = []auth.Mount{{
			Name: seg, Kind: "bucket", Remote: "tos1", Prefix: "proj/raw", Target: "/workspace/" + seg, Mode: "ro",
		}}
		plan, err := Build(in)
		if err != nil {
			t.Fatalf("bucket target %s: %v", seg, err)
		}
		if strings.Contains(plan.Wrapper, "ra w") || strings.Contains(plan.Wrapper, ";id") || strings.Contains(plan.Wrapper, "`") {
			t.Fatalf("accepted wrapper is not limited to a safe name:\n%s", plan.Wrapper)
		}
	}
	for _, seg := range []string{"ra w", "raw;id", "../x"} {
		for _, kind := range []string{"path", "bucket"} {
			target := "/data/" + seg
			in := baseInput(t)
			in.Remotes = remote
			in.Mounts = []auth.Mount{{
				Name: "raw", Kind: kind, Host: rootHost(t, in, "data", "ref"), Remote: "tos1", Prefix: "proj/raw", Target: target, Mode: "ro",
			}}
			if _, err := Build(in); err == nil {
				t.Fatalf("%s target %q was accepted", kind, target)
			}
		}
	}
}

func TestWriteRcloneConfig(t *testing.T) {
	dir := t.TempDir()
	lineCred := filepath.Join(dir, "line")
	if err := os.WriteFile(lineCred, []byte("access_key_id=AKIAline\nsecret_access_key=line-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yamlCred := filepath.Join(dir, "yaml")
	if err := os.WriteFile(yamlCred, []byte("accessKey: AKIAyaml\nsecretKey: yaml-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cred, access, secret, provider string
	}{
		{"tos1", lineCred, "AKIAline", "line-secret", "tos"},
		{"s3main", yamlCred, "AKIAyaml", "yaml-secret", "s3"},
	} {
		dst := filepath.Join(dir, "run", tc.name+".rclone.conf")
		remote := Remote{
			Name: tc.name, Provider: tc.provider, Endpoint: "https://example.test", Region: "cn-beijing",
			Bucket: "bucket", CredentialFile: tc.cred,
		}
		if err := WriteRcloneConfig(dst, remote); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode: %o", fi.Mode().Perm())
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range []string{
			"[" + tc.name + "]",
			"type = s3",
			"provider = Other",
			"endpoint = https://example.test",
			"region = cn-beijing",
			"force_path_style = false",
			"access_key_id = " + tc.access,
			"secret_access_key = " + tc.secret,
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("config missing %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, tc.cred) {
			t.Fatalf("credential path leaked into rclone config: %s", text)
		}
	}
}

func TestLoadRootsAndRemotes(t *testing.T) {
	dir := t.TempDir()
	roots, err := LoadRoots(filepath.Join(dir, "missing.yaml"))
	if err != nil || roots != nil {
		t.Fatalf("missing roots: %+v %v", roots, err)
	}
	path := filepath.Join(dir, "roots.yaml")
	body := []byte("roots:\n  - id: data\n    host: /srv/data\n    allowRw: false\n  - id: work\n    host: /srv/work\n    allowRw: true\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err = LoadRoots(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[1].ID != "work" || roots[1].Host != "/srv/work" || !roots[1].AllowRW || roots[0].AllowRW {
		t.Fatalf("roots: %+v", roots)
	}
	rpath := filepath.Join(dir, "remotes.yaml")
	if err := os.WriteFile(rpath, []byte("remotes:\n  - name: tos1\n    provider: tos\n    endpoint: https://tos.example\n    region: cn-beijing\n    bucket: data\n    credentialFile: /tmp/cred\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remotes, err := LoadRemotes(rpath)
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 1 || remotes[0].Name != "tos1" || remotes[0].Bucket != "data" || remotes[0].CredentialFile != "/tmp/cred" {
		t.Fatalf("remotes: %+v", remotes)
	}
}

func TestBuildRejectsSymlinkEscape(t *testing.T) {
	in := baseInput(t)
	outside := t.TempDir()
	link := filepath.Join(in.Roots[0].Host, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	in.Mounts = []auth.Mount{{
		Name: "escape", Kind: "path", Host: link, Target: "/data/escape", Mode: "ro",
	}}
	if _, err := Build(in); err == nil {
		t.Fatal("a symlink escaping its root was accepted")
	}
	if err := ValidateMount(in.Mounts[0], in.Roots); err == nil {
		t.Fatal("ValidateMount accepted a symlink escape")
	}
}

func TestBuildBindsPhysicalHostPath(t *testing.T) {
	in := baseInput(t)
	target := rootHost(t, in, "data", "real")
	link := filepath.Join(in.Roots[0].Host, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	in.Mounts = []auth.Mount{{
		Name: "link", Kind: "path", Host: link, Target: "/data/link", Mode: "ro",
	}}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--ro-bind", target, "/data/link") {
		t.Fatalf("bind must use the physical path: %#v", plan.BwrapArgv)
	}
}

func TestValidateMountRejectsWorkspaceRoot(t *testing.T) {
	in := baseInput(t)
	host := rootHost(t, in, "work", "proj")
	for _, m := range []auth.Mount{
		{Name: "x", Kind: "path", Host: host, Target: "/workspace", Mode: "ro"},
		{Name: "x", Kind: "path", Host: host, Target: "/workspace", Mode: "rw"},
	} {
		if err := ValidateMount(m, in.Roots); err == nil {
			t.Fatalf("target /workspace was accepted: %+v", m)
		}
	}
}

func TestBuildAlwaysSetsSandboxPATH(t *testing.T) {
	in := baseInput(t)
	in.Dist = "/opt/dw/dist/default"
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	var path string
	found := false
	for _, kv := range plan.Env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
			found = true
		}
	}
	if !found || path == "" {
		t.Fatalf("PATH must always be planned: %#v", plan.Env)
	}
	if strings.HasPrefix(path, sep) || strings.HasSuffix(path, sep) || strings.Contains(path, sep+sep) {
		t.Fatalf("PATH has an empty component: %q", path)
	}
	for _, part := range strings.Split(path, sep) {
		if !filepath.IsAbs(part) {
			t.Fatalf("PATH component %q is not absolute: %q", part, path)
		}
	}
	if !strings.HasSuffix(path, sep+"/usr/local/bin"+sep+"/usr/bin"+sep+"/bin") {
		t.Fatalf("PATH tail: %q", path)
	}
}

func TestBuildPrivateDevShm(t *testing.T) {
	in := baseInput(t)
	in.Dist = "/opt/dw/dist/default"
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSeq(plan.BwrapArgv, "--dev", "/dev", "--tmpfs", "/dev/shm") {
		t.Fatalf("private /dev/shm must follow --dev /dev: %#v", plan.BwrapArgv)
	}
}
