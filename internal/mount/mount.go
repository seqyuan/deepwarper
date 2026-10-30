package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/pathutil"
)

// ErrEmptyPlan is returned when there is nothing to sandbox, so the caller
// can start the process without bwrap.
var ErrEmptyPlan = errors.New("mount: empty plan")

// Root is a host directory the admin allows users to mount.
type Root struct {
	ID      string `yaml:"id"`
	Host    string `yaml:"host"`
	AllowRW bool   `yaml:"allowRw"`
}

// Remote is one cloud bucket the admin has credentials for.
type Remote struct {
	Name           string `yaml:"name"`
	Provider       string `yaml:"provider"`
	Endpoint       string `yaml:"endpoint"`
	Region         string `yaml:"region"`
	Bucket         string `yaml:"bucket"`
	CredentialFile string `yaml:"credentialFile"`
}

// RcloneMount is one rclone mount the runtime should start before bwrap.
type RcloneMount struct {
	Name       string
	Argv       []string
	MountPoint string
}

// Plan is a pure description. Build does not execute bwrap or rclone.
type Plan struct {
	BwrapArgv []string
	Rclone    []RcloneMount
	Env       []string
	// Wrapper is the in-sandbox shell script that starts rclone, waits until
	// the mount is visible, runs dsh, and unmounts. Empty when there are no
	// bucket mounts.
	Wrapper string
}

// Input is everything Build needs. Bwrap and RcloneBin default to "bwrap"
// and "rclone" when empty.
type Input struct {
	Moniforge string
	DSHHome   string
	Workspace string
	Dist      string
	Roots     []Root
	Remotes   []Remote
	Mounts    []auth.Mount
	Bwrap     string
	RcloneBin string
	FuseDir   string
	// BinDir, when set, contributes dw-env, bind-mounted read-only at
	// /opt/dw/env/dw-env so it does not cover the dsh binary directory.
	BinDir string
	// PatchFile is the host runtime patch, bind-mounted alone at
	// /dw/control/<filename>. The rest of the control directory stays out.
	PatchFile string
	// HostBinds are host paths the caller has decided exist. Each is
	// read-only bind-mounted onto the same path. Build does not stat them.
	HostBinds []string
	Command   []string
	// ResolvePath resolves a host path physically before it is trusted.
	// Defaults to pathutil.Resolve; tests inject a fake to avoid real mounts.
	ResolvePath func(string) (string, error)
}

type rootsFile struct {
	Roots []Root `yaml:"roots"`
}
type remotesFile struct {
	Remotes []Remote `yaml:"remotes"`
}

// ConfigPath is the per-mount rclone config under the fuse runtime directory.
func ConfigPath(fuseDir, name string) string {
	return filepath.Join(fuseDir, name+".rclone.conf")
}

// LoadRoots reads roots.yaml. A missing file is an empty list.
func LoadRoots(path string) ([]Root, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f rootsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Roots, nil
}

// LoadRemotes reads remotes.yaml. A missing file is an empty list.
func LoadRemotes(path string) ([]Remote, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f remotesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Remotes, nil
}

// SaveRemotes writes remotes.yaml.
func SaveRemotes(path string, remotes []Remote) error {
	if remotes == nil {
		remotes = []Remote{}
	}
	data, err := yaml.Marshal(remotesFile{Remotes: remotes})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Build plans the private view. It does not run bwrap or rclone.
func Build(in Input) (Plan, error) {
	if len(in.Mounts) == 0 && in.Moniforge == "" && in.Dist == "" {
		return Plan{}, ErrEmptyPlan
	}
	if in.Workspace == "" || in.DSHHome == "" {
		return Plan{}, errors.New("mount: workspace and dsh home are required")
	}
	if in.FuseDir == "" && hasBucket(in.Mounts) {
		return Plan{}, errors.New("mount: fuse directory is required")
	}
	bwrap := in.Bwrap
	if bwrap == "" {
		bwrap = "bwrap"
	}
	rcloneBin := in.RcloneBin
	if rcloneBin == "" {
		rcloneBin = "rclone"
	}

	argv := []string{
		bwrap,
		"--die-with-parent",
		"--chdir", "/workspace",
		"--unshare-user",
		"--unshare-pid",
		"--proc", "/proc",
		"--dev", "/dev",
		// A private shm keeps node/uv thread pools and tool IPC working without
		// exposing the host's shared memory.
		"--tmpfs", "/dev/shm",
		"--tmpfs", "/tmp",
	}
	for _, p := range in.HostBinds {
		if p == "" {
			continue
		}
		argv = append(argv, "--ro-bind", p, p)
	}
	if in.Moniforge != "" {
		argv = append(argv, "--ro-bind", in.Moniforge, in.Moniforge)
	}
	argv = append(argv, "--bind", in.Workspace, "/workspace")
	// Home is visible as /home/dsh (DSH_HOME) and again at the host path so
	// absolute plugin paths written into user-cordis.patch.yml still resolve.
	argv = append(argv, "--bind", in.DSHHome, "/home/dsh")
	if filepath.Clean(in.DSHHome) != "/home/dsh" {
		argv = append(argv, "--bind", in.DSHHome, in.DSHHome)
	}
	if in.Dist != "" {
		// The profile's node_modules symlink uses the absolute dist path.
		argv = append(argv, "--ro-bind", in.Dist, in.Dist)
		if filepath.Clean(in.Dist) != "/opt/dw/dist" {
			argv = append(argv, "--ro-bind", in.Dist, "/opt/dw/dist")
		}
	}
	if in.BinDir != "" {
		argv = append(argv, "--ro-bind", filepath.Join(in.BinDir, "dw-env"), "/opt/dw/env/dw-env")
	}
	if in.PatchFile != "" {
		// Only the runtime patch crosses into the sandbox. spec.json and the
		// dsh token stay on the host.
		argv = append(argv, "--ro-bind", in.PatchFile, "/dw/control/"+filepath.Base(in.PatchFile))
	}

	var rclone []RcloneMount
	resolve := in.ResolvePath
	if resolve == nil {
		resolve = pathutil.Resolve
	}
	for _, m := range in.Mounts {
		switch m.Kind {
		case "path":
			if err := checkMountName(m.Name); err != nil {
				return Plan{}, err
			}
			if _, err := cleanHost(m.Host); err != nil {
				return Plan{}, err
			}
			if err := checkTarget(m.Target, m.Mode); err != nil {
				return Plan{}, err
			}
			root, host, err := physicalRoot(m.Host, in.Roots, resolve)
			if err != nil {
				return Plan{}, err
			}
			if m.Mode == "rw" && !root.AllowRW {
				return Plan{}, fmt.Errorf("mount: root %s does not allow rw", root.ID)
			}
			if strings.HasPrefix(m.Target, "/data/") && !hasSeqArg(argv, "--dir", "/data") {
				argv = append(argv, "--dir", "/data")
			}
			flag := "--ro-bind"
			if m.Mode == "rw" {
				flag = "--bind"
			}
			argv = append(argv, flag, host, m.Target)
		case "bucket":
			rc, hostPoint, err := planBucket(in, m, rcloneBin)
			if err != nil {
				return Plan{}, err
			}
			if strings.HasPrefix(m.Target, "/data/") && !hasSeqArg(argv, "--dir", "/data") {
				argv = append(argv, "--dir", "/data")
			}
			flag := "--ro-bind"
			if m.Mode == "rw" {
				flag = "--bind"
			}
			argv = append(argv, flag, hostPoint, m.Target)
			rclone = append(rclone, rc)
		default:
			return Plan{}, fmt.Errorf("mount: kind %q is not path or bucket", m.Kind)
		}
	}
	if len(in.Command) > 0 && filepath.IsAbs(in.Command[0]) {
		dir := filepath.Dir(in.Command[0])
		if dir != "" && dir != "/" && !bindCovers(argv, dir) {
			argv = append(argv, "--ro-bind", dir, dir)
		}
	}

	if len(in.Command) > 0 {
		argv = append(argv, "--")
		argv = append(argv, in.Command...)
	}

	env := []string{"HOME=/home/dsh", "DSH_HOME=/home/dsh", "PATH=" + sandboxPATH(in)}
	return Plan{BwrapArgv: argv, Rclone: rclone, Env: env}, nil
}

// sandboxPATH is dsh's bin directory, then moniforge's bin, then /opt/dw/env
// when dw-env is installed there, then the system bins. Every component is
// absolute and deduplicated; an empty or relative entry would let PATH lookup
// resolve against the workspace.
func sandboxPATH(in Input) string {
	var parts []string
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		if p == string(filepath.Separator) {
			return
		}
		for _, existing := range parts {
			if existing == p {
				return
			}
		}
		parts = append(parts, p)
	}
	if len(in.Command) > 0 {
		add(filepath.Dir(in.Command[0]))
	}
	if in.Moniforge != "" {
		add(filepath.Join(in.Moniforge, "bin"))
	}
	if in.BinDir != "" {
		add("/opt/dw/env")
	}
	// The wrapper calls rclone, grep, sleep, fusermount, and umount by path
	// or by name. Keep those directories after the toolchain bins.
	for _, p := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		add(p)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// RcloneReadyTimeout is how long the parent waits for a host fuse mount.
const RcloneReadyTimeout = 30 * time.Second

// FuseReady reports whether mountinfo has a fuse filesystem mounted at path.
// A non-fuse bind of the same path, including the workspace, does not count.
func FuseReady(info, path string) bool {
	path = filepath.Clean(path)
	for _, line := range strings.Split(info, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 6 {
			continue
		}
		fst := ""
		for i, f := range fields {
			if f == "-" && i+1 < len(fields) {
				fst = fields[i+1]
				break
			}
		}
		if filepath.Clean(fields[4]) == path && strings.Contains(fst, "fuse") {
			return true
		}
	}
	return false
}

// WaitFuse polls until FuseReady is true or the timeout elapses.
func WaitFuse(path string, timeout time.Duration, read func() (string, error), sleep func(time.Duration)) error {
	if timeout <= 0 {
		timeout = RcloneReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		info, err := read()
		if err == nil && FuseReady(info, path) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("rclone mount %s was not ready", path)
		}
		if sleep != nil {
			sleep(100 * time.Millisecond)
		}
	}
}

// EnsureMountpoint creates the host directory rclone will mount on.
func EnsureMountpoint(path string) error {
	if path == "" {
		return errors.New("mount: empty mountpoint")
	}
	return os.MkdirAll(path, 0o700)
}

func hasBucket(mounts []auth.Mount) bool {
	for _, m := range mounts {
		if m.Kind == "bucket" {
			return true
		}
	}
	return false
}

func planBucket(in Input, m auth.Mount, rcloneBin string) (RcloneMount, string, error) {
	if m.Target == "/workspace" || m.Target == "/data" {
		return RcloneMount{}, "", fmt.Errorf("mount: bucket target %q is reserved", m.Target)
	}
	if err := checkTarget(m.Target, m.Mode); err != nil {
		return RcloneMount{}, "", err
	}
	if !strings.HasPrefix(m.Target, "/data/") && !strings.HasPrefix(m.Target, "/workspace/") {
		return RcloneMount{}, "", fmt.Errorf("mount: bucket target %q must be /data/<name> or /workspace/<name>", m.Target)
	}
	if err := checkPrefix(m.Prefix); err != nil {
		return RcloneMount{}, "", err
	}
	if err := checkMountName(m.Name); err != nil {
		return RcloneMount{}, "", err
	}
	var remote Remote
	found := false
	for _, r := range in.Remotes {
		if r.Name == m.Remote {
			remote = r
			found = true
			break
		}
	}
	if !found {
		return RcloneMount{}, "", fmt.Errorf("mount: unknown remote %q", m.Remote)
	}
	if strings.TrimSpace(remote.Bucket) == "" {
		return RcloneMount{}, "", fmt.Errorf("mount: remote %q has no bucket", remote.Name)
	}
	hostPoint := filepath.Join(in.FuseDir, m.Name)
	hostCfg := ConfigPath(in.FuseDir, m.Name)
	src := remote.Name + ":" + remote.Bucket + "/" + m.Prefix
	// rclone runs in the parent. --allow-other lets the user namespace read
	// the fuse mount. The config path stays in this argv, not in bwrap.
	rcArgv := []string{rcloneBin, "--config", hostCfg, "mount", src, hostPoint, "--allow-other"}
	if m.Mode == "ro" {
		rcArgv = append(rcArgv, "--read-only")
	}
	return RcloneMount{Name: m.Name, Argv: rcArgv, MountPoint: hostPoint}, hostPoint, nil
}

// ValidateMount applies the target, name, and root checks Build uses, with the
// same physical resolution: a symlink under a root may not escape it.
// A path mount must sit under exactly one root. A bucket mount does not.
func ValidateMount(m auth.Mount, roots []Root) error {
	return validateMountWith(m, roots, pathutil.Resolve)
}

func validateMountWith(m auth.Mount, roots []Root, resolve func(string) (string, error)) error {
	if err := checkMountName(m.Name); err != nil {
		return err
	}
	switch m.Kind {
	case "path":
		if _, err := cleanHost(m.Host); err != nil {
			return err
		}
		if err := checkTarget(m.Target, m.Mode); err != nil {
			return err
		}
		root, _, err := physicalRoot(m.Host, roots, resolve)
		if err != nil {
			return err
		}
		if m.Mode == "rw" && !root.AllowRW {
			return fmt.Errorf("mount: root %s does not allow rw", root.ID)
		}
		return nil
	case "bucket":
		if m.Target == "/workspace" || m.Target == "/data" {
			return fmt.Errorf("mount: bucket target %q is reserved", m.Target)
		}
		if err := checkTarget(m.Target, m.Mode); err != nil {
			return err
		}
		return checkPrefix(m.Prefix)
	default:
		return fmt.Errorf("mount: kind %q is not path or bucket", m.Kind)
	}
}

// ValidName reports whether s can be a mount or remote name.
func ValidName(s string) bool {
	return mountNameRE.MatchString(s)
}

func checkTarget(target, mode string) error {
	if mode != "ro" && mode != "rw" {
		return fmt.Errorf("mount: mode %q is not ro or rw", mode)
	}
	rest, ok := strings.CutPrefix(target, "/data/")
	if !ok {
		rest, ok = strings.CutPrefix(target, "/workspace/")
	}
	if !ok || !singleSegment(rest) {
		// "/workspace" itself is reserved for the private workspace bind;
		// a mount there would silently shadow it.
		return fmt.Errorf("mount: invalid target %q", target)
	}
	if mode == "rw" && !strings.HasPrefix(target, "/workspace/") {
		return fmt.Errorf("mount: rw is only allowed under /workspace/<name> (target %q)", target)
	}
	return nil
}

// mountNameRE is the only spelling Build will interpolate into a shell
// script: one leading alphanumeric, then letters, digits, underscore, or
// hyphen, at most 32 characters.
var mountNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

func checkMountName(name string) error {
	if !mountNameRE.MatchString(name) {
		return fmt.Errorf("mount: invalid name %q", name)
	}
	return nil
}

func singleSegment(s string) bool {
	return mountNameRE.MatchString(s)
}

func checkPrefix(prefix string) error {
	if prefix == "" || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "..") || strings.Contains(prefix, "\\") {
		return fmt.Errorf("mount: invalid bucket prefix %q", prefix)
	}
	for _, seg := range strings.Split(prefix, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("mount: invalid bucket prefix %q", prefix)
		}
	}
	return nil
}

func cleanHost(host string) (string, error) {
	if strings.TrimSpace(host) == "" {
		return "", errors.New("mount: empty host path")
	}
	for _, p := range strings.Split(filepath.ToSlash(host), "/") {
		if p == ".." {
			return "", fmt.Errorf("mount: path %q contains ..", host)
		}
	}
	cleaned := filepath.Clean(host)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("mount: host path %q escapes the configured roots", host)
	}
	return cleaned, nil
}

// physicalRoot resolves the host and every root, then requires exactly one
// containing root. The returned host path is the physical path that is bound,
// so a symlink inside a root cannot redirect the bind elsewhere.
func physicalRoot(host string, roots []Root, resolve func(string) (string, error)) (Root, string, error) {
	realHost, err := resolve(host)
	if err != nil {
		return Root{}, "", fmt.Errorf("mount: host path %q: %w", host, err)
	}
	var match []Root
	seen := map[string]bool{}
	for _, r := range roots {
		if strings.TrimSpace(r.Host) == "" {
			continue
		}
		realRoot, rerr := resolve(r.Host)
		if rerr != nil {
			return Root{}, "", fmt.Errorf("mount: root %s: %w", r.ID, rerr)
		}
		if seen[realRoot] {
			continue
		}
		seen[realRoot] = true
		if pathutil.Within(realHost, realRoot) {
			match = append(match, r)
		}
	}
	if len(match) != 1 {
		return Root{}, "", fmt.Errorf("mount: %q is under %d roots, want exactly one", realHost, len(match))
	}
	return match[0], realHost, nil
}

func hasSeqArg(argv []string, parts ...string) bool {
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

func bindCovers(argv []string, path string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] != "--ro-bind" && argv[i] != "--bind" {
			continue
		}
		host := argv[i+1]
		if path == host || strings.HasPrefix(path, host+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// WriteRcloneConfig writes a 0600 rclone config that contains only remote.
// Credentials are read from remote.CredentialFile and are not left as a path
// inside the config.
func WriteRcloneConfig(dst string, remote Remote) error {
	switch remote.Provider {
	case "tos", "s3":
	default:
		return fmt.Errorf("mount: provider %q is not tos or s3", remote.Provider)
	}
	access, secret, err := readCredentials(remote.CredentialFile)
	if err != nil {
		return err
	}
	if remote.Name == "" {
		return errors.New("mount: remote name is required")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", remote.Name)
	b.WriteString("type = s3\n")
	b.WriteString("provider = Other\n")
	fmt.Fprintf(&b, "access_key_id = %s\n", access)
	fmt.Fprintf(&b, "secret_access_key = %s\n", secret)
	fmt.Fprintf(&b, "endpoint = %s\n", remote.Endpoint)
	fmt.Fprintf(&b, "region = %s\n", remote.Region)
	b.WriteString("force_path_style = false\n")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

func readCredentials(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("mount: credential file: %w", err)
	}
	text := string(data)
	var access, secret string
	if strings.Contains(text, "access_key_id=") || strings.Contains(text, "secret_access_key=") {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "access_key_id=") {
				access = strings.TrimPrefix(line, "access_key_id=")
			}
			if strings.HasPrefix(line, "secret_access_key=") {
				secret = strings.TrimPrefix(line, "secret_access_key=")
			}
		}
	}
	if access == "" || secret == "" {
		var y struct {
			AccessKey string `yaml:"accessKey"`
			SecretKey string `yaml:"secretKey"`
		}
		if err := yaml.Unmarshal(data, &y); err != nil {
			return "", "", fmt.Errorf("mount: credential file: %w", err)
		}
		if access == "" {
			access = y.AccessKey
		}
		if secret == "" {
			secret = y.SecretKey
		}
	}
	if access == "" || secret == "" {
		return "", "", errors.New("mount: credential file missing access key or secret")
	}
	return access, secret, nil
}
