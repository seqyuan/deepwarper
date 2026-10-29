package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seqyuan/deepwarper/internal/mount"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/state"
)

type Spec struct {
	InstanceID  string `json:"instance_id"`
	Incarnation string `json:"incarnation"`
	// UnitName is the systemd unit the control plane assigned when the
	// local backend runs in unit mode. Informational on the agent side.
	UnitName    string `json:"unit_name,omitempty"`
	Profile     string `json:"profile"`
	DshBin      string `json:"dsh_bin"`
	Home        string `json:"home"`
	Workspace   string `json:"workspace"`
	DistPath    string `json:"dist_path,omitempty"`
	DistVersion string `json:"dist_version,omitempty"`
	ControlDir  string `json:"control_dir"`
	LogPath     string `json:"log_path"`
	PublicURL   string `json:"public_url"`
	TrustedHost string `json:"trusted_host"`
	// Port is the loopback port dsh binds. Zero means pick a free loopback port.
	Port         int    `json:"port"`
	PatchPath    string `json:"patch_path,omitempty"`
	PatchContent string `json:"patch_content,omitempty"`
	Heartbeat    string `json:"heartbeat,omitempty"`
	// ReadyTimeout replaces the old hardcoded 90s wait for dsh's ready line.
	ReadyTimeout string `json:"ready_timeout,omitempty"`
	CPU          int    `json:"cpu,omitempty"`
	Memory       string `json:"memory,omitempty"`
	Walltime     string `json:"walltime,omitempty"`
	GPU          int    `json:"gpu,omitempty"`
	// BwrapArgv, when non-empty, is the sandbox prefix. The agent runs dsh
	// as the command inside it. Rclone mounts are started on the host first
	// so the fuse directories exist for those binds.
	BwrapArgv []string     `json:"bwrap_argv,omitempty"`
	Rclone    []RcloneSpec `json:"rclone,omitempty"`
	Env       []string     `json:"env,omitempty"`
	// Wrapper is the in-sandbox script. It is stored on the spec because
	// rendezvous.Clear deletes control-dir files before the agent runs.
	Wrapper string `json:"wrapper,omitempty"`
}

// RcloneSpec is one host-side rclone mount stored in spec.json.
type RcloneSpec struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	MountPoint string   `json:"mount_point"`
}

const (
	ExitOK                = 0
	ExitHomeLocked        = 3
	ExitDshMissing        = 4
	ExitExitedBeforeReady = 5
	ExitReadyTimeout      = 6
	ExitNoToken           = 7
	ExitControlDir        = 8
)

func Run(ctx context.Context, specPath string) int {
	data, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read spec: %v\n", err)
		return ExitControlDir
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		fmt.Fprintf(os.Stderr, "parse spec: %v\n", err)
		return ExitControlDir
	}
	if spec.ControlDir == "" || spec.Home == "" || spec.Workspace == "" {
		fmt.Fprintln(os.Stderr, "spec is missing control_dir/home/workspace")
		return ExitControlDir
	}
	// Port 0 means this process picks a free loopback port and publishes it
	// in ready.json.
	if spec.Port == 0 {
		port, err := freeLoopbackPort()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pick a loopback port: %v\n", err)
			_ = rendezvous.WriteString(spec.ControlDir, "state", "failed", 0o644)
			_ = rendezvous.WriteString(spec.ControlDir, "exit_code", fmt.Sprint(ExitControlDir), 0o644)
			return ExitControlDir
		}
		spec.Port = port
	}
	if err := os.MkdirAll(spec.ControlDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "control dir: %v\n", err)
		return ExitControlDir
	}
	if spec.PatchContent != "" {
		spec.PatchPath = filepath.Join(spec.ControlDir, "runtime.patch.yml")
		if err := os.WriteFile(spec.PatchPath, []byte(spec.PatchContent), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write runtime patch: %v\n", err)
			return ExitControlDir
		}
	}
	if err := os.MkdirAll(spec.Home, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "home: %v\n", err)
		return ExitControlDir
	}
	if err := os.MkdirAll(spec.Workspace, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "workspace: %v\n", err)
		return ExitControlDir
	}

	lock, err := state.Acquire(filepath.Join(spec.Home, ".dw.lock"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "home lock: %v\n", err)
		return ExitHomeLocked
	}
	defer lock.Release()

	pub := func(name, value string) {
		_ = rendezvous.WriteString(spec.ControlDir, name, value, 0o644)
	}
	pub("incarnation", spec.Incarnation)
	pub("state", "starting")
	_ = rendezvous.WriteString(spec.ControlDir, "pid", fmt.Sprint(os.Getpid()), 0o644)
	if start := procStartTime(os.Getpid()); start > 0 {
		_ = rendezvous.WriteString(spec.ControlDir, "pid_start", fmt.Sprint(start), 0o644)
	}

	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log file: %v\n", err)
		pub("state", "failed")
		return ExitControlDir
	}
	defer logFile.Close()
	logger := &redactingWriter{w: logFile}

	if _, err := exec.LookPath(spec.DshBin); err != nil {
		fmt.Fprintf(logger, "dsh binary not found: %v\n", err)
		pub("state", "failed")
		pub("exit_code", fmt.Sprint(ExitDshMissing))
		return ExitDshMissing
	}

	argv, err := prefixCommand(spec, exec.LookPath)
	if err != nil {
		fmt.Fprintf(logger, "%v\n", err)
		pub("state", "failed")
		return 1
	}
	// Clear already removed anything attachSandbox wrote. Rewrite the wrapper
	// from the spec immediately before bwrap starts.
	if err := writeWrapper(spec); err != nil {
		fmt.Fprintf(logger, "%v\n", err)
		pub("state", "failed")
		return 1
	}
	var fuseMounts []string
	if len(spec.Rclone) > 0 {
		var err error
		fuseMounts, err = startHostRclone(spec)
		if err != nil {
			fmt.Fprintf(logger, "%v\n", err)
			pub("state", "failed")
			return 1
		}
		defer stopHostRclone(fuseMounts)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Workspace
	if len(spec.BwrapArgv) > 0 {
		cmd.Env = sandboxEnv(os.Environ(), spec.Env)
	} else {
		cmd.Env = append(os.Environ(),
			"DSH_HOME="+spec.Home,
			"HOME="+spec.Home,
			"DSH_PERMISSION_MODE=workspace-write",
		)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(logger, "stdout pipe: %v\n", err)
		pub("state", "failed")
		return ExitControlDir
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(logger, "stderr pipe: %v\n", err)
		pub("state", "failed")
		return ExitControlDir
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logger, "start dsh: %v\n", err)
		pub("state", "failed")
		pub("exit_code", fmt.Sprint(ExitDshMissing))
		return ExitDshMissing
	}

	readyCh := make(chan string, 1)
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	go scan(stdout, logger, readyCh, true)
	go scan(stderr, logger, nil, false)

	// SIGTERM must be handled before readiness too: a systemd unit with
	// KillMode=control-group delivers it during `starting` as well.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	heartbeat := 30 * time.Second
	if spec.Heartbeat != "" {
		if d, err := time.ParseDuration(spec.Heartbeat); err == nil && d > 0 {
			heartbeat = d
		}
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = rendezvous.WriteString(spec.ControlDir, "heartbeat", time.Now().UTC().Format(time.RFC3339Nano), 0o644)
			}
		}
	}()

	var token string
	readyTimeout := 90 * time.Second
	if spec.ReadyTimeout != "" {
		if d, err := time.ParseDuration(spec.ReadyTimeout); err == nil && d > 0 {
			readyTimeout = d
		}
	}
	select {
	case line := <-readyCh:
		token, err = parseReadyToken(line)
		if err != nil {
			fmt.Fprintf(logger, "parse dsh ready line: %v\n", err)
			killGroup(cmd)
			pub("state", "failed")
			pub("exit_code", fmt.Sprint(ExitNoToken))
			return ExitNoToken
		}
	case err := <-exitCh:
		code := exitCode(err)
		fmt.Fprintf(logger, "dsh exited before ready: %v\n", err)
		pub("state", "exited")
		pub("exit_code", fmt.Sprint(code))
		return ExitExitedBeforeReady
	case s := <-sig:
		fmt.Fprintf(logger, "received %s before ready, stopping dsh\n", s)
		killGroup(cmd)
		select {
		case <-exitCh:
		case <-time.After(5 * time.Second):
		}
		pub("state", "failed")
		pub("exit_code", "143")
		return 143
	case <-time.After(readyTimeout):
		fmt.Fprintf(logger, "timed out waiting for dsh ready line after %s\n", readyTimeout)
		killGroup(cmd)
		pub("state", "failed")
		pub("exit_code", fmt.Sprint(ExitReadyTimeout))
		return ExitReadyTimeout
	}

	if err := rendezvous.WriteString(spec.ControlDir, "token", token, 0o600); err != nil {
		fmt.Fprintf(logger, "write token: %v\n", err)
		killGroup(cmd)
		pub("state", "failed")
		return ExitControlDir
	}
	r := rendezvous.Ready{
		Version:     1,
		Incarnation: spec.Incarnation,
		State:       "ready",
		Host:        "127.0.0.1",
		Port:        spec.Port,
		PID:         cmd.Process.Pid,
		Profile:     spec.Profile,
		PublishedAt: time.Now().UTC(),
	}
	if host, err := os.Hostname(); err == nil {
		r.Node = host
	}
	if err := rendezvous.WriteReady(spec.ControlDir, r); err != nil {
		fmt.Fprintf(logger, "write ready: %v\n", err)
		killGroup(cmd)
		pub("state", "failed")
		return ExitControlDir
	}
	pub("state", "ready")
	fmt.Fprintf(logger, "dsh ready on 127.0.0.1:%d (profile=%s)\n", spec.Port, spec.Profile)

	select {
	case s := <-sig:
		fmt.Fprintf(logger, "received %s, stopping dsh\n", s)
		killGroup(cmd)
		<-exitCh
		pub("state", "exited")
		pub("exit_code", "143")
		return 143
	case err := <-exitCh:
		code := exitCode(err)
		pub("state", "exited")
		pub("exit_code", fmt.Sprint(code))
		fmt.Fprintf(logger, "dsh exited with code %d\n", code)
		return code
	}
}

// sandboxEnv is the only environment a bwrap child receives. Host secrets
// such as DW_PASSWORD are not forwarded.
func sandboxEnv(host, planned []string) []string {
	keep := map[string]string{}
	take := func(kv string, limit bool) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return
		}
		if limit && k != "LANG" && k != "LC_ALL" {
			return
		}
		switch k {
		case "PATH", "HOME", "DSH_HOME", "LANG", "LC_ALL":
			keep[k] = v
		}
	}
	for _, kv := range host {
		take(kv, true)
	}
	for _, kv := range planned {
		take(kv, false)
	}
	var out []string
	for _, k := range []string{"PATH", "HOME", "DSH_HOME", "LANG", "LC_ALL"} {
		if v, ok := keep[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func resolveRcloneArgv(argv []string, look func(string) (string, error)) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("rclone argv is empty")
	}
	out := append([]string{}, argv...)
	if filepath.IsAbs(out[0]) {
		return out, nil
	}
	p, err := look(out[0])
	if err != nil {
		return nil, fmt.Errorf("rclone requested but not found: %w", err)
	}
	out[0] = p
	return out, nil
}

func readMountinfo() (string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func startHostRclone(spec Spec) ([]string, error) {
	var points []string
	var cmds []*exec.Cmd
	fail := func(err error) ([]string, error) {
		stopHostRclone(points)
		for _, cmd := range cmds {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
			}
		}
		return nil, err
	}
	for _, rc := range spec.Rclone {
		if err := os.MkdirAll(rc.MountPoint, 0o700); err != nil {
			return fail(err)
		}
		argv, err := resolveRcloneArgv(rc.Argv, exec.LookPath)
		if err != nil {
			return fail(err)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return fail(fmt.Errorf("rclone mount %s: %w", rc.Name, err))
		}
		cmds = append(cmds, cmd)
		if err := mount.WaitFuse(rc.MountPoint, mount.RcloneReadyTimeout, readMountinfo, time.Sleep); err != nil {
			return fail(err)
		}
		points = append(points, rc.MountPoint)
	}
	return points, nil
}

func stopHostRclone(points []string) {
	for _, p := range points {
		_ = exec.Command("fusermount", "-u", p).Run()
		_ = exec.Command("umount", p).Run()
	}
}

func writeWrapper(spec Spec) error {
	if spec.Wrapper == "" {
		return nil
	}
	if spec.ControlDir == "" {
		return errors.New("wrapper requires a control directory")
	}
	if err := os.MkdirAll(spec.ControlDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(spec.ControlDir, "dw-wrap.sh")
	if err := os.WriteFile(path, []byte(spec.Wrapper), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// dshArgv is the process the agent would exec when bwrap is not in use.
func dshArgv(spec Spec) []string {
	args := []string{spec.DshBin, "--profile", spec.Profile}
	patch := spec.PatchPath
	if len(spec.BwrapArgv) > 0 && patch != "" {
		// The control directory is bind-mounted at /dw/control.
		patch = "/dw/control/" + filepath.Base(patch)
	}
	if patch != "" {
		// --patch is a launcher option; it must precede app args.
		args = append(args, "--patch", patch)
	}
	args = append(args,
		"--no-open",
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(spec.Port),
		"--public-url", spec.PublicURL,
		"--trusted-host", spec.TrustedHost,
	)
	return args
}

// prefixCommand returns the dsh argv, or that argv inside bwrap when the
// spec carries a sandbox prefix. lookPath is called only when bwrap is
// requested. A missing bwrap is an error.
func prefixCommand(spec Spec, lookPath func(string) (string, error)) ([]string, error) {
	cmd := dshArgv(spec)
	if len(spec.BwrapArgv) == 0 {
		return cmd, nil
	}
	name := strings.TrimSpace(spec.BwrapArgv[0])
	if name == "" {
		name = "bwrap"
	}
	resolved, err := lookPath(name)
	if err != nil {
		return nil, fmt.Errorf("bwrap requested but not found: %w", err)
	}
	prefix := append([]string{}, spec.BwrapArgv...)
	prefix[0] = resolved
	cut := len(prefix)
	for i, a := range prefix {
		if a == "--" {
			cut = i
			break
		}
	}
	var planned []string
	if cut < len(prefix) {
		planned = prefix[cut+1:]
	}
	prefix = append(prefix[:cut], "--")
	// A bucket sandbox's inner command is the wrapper. Keep it and let it
	// execute dsh. Any other planned tail is replaced by the live dsh argv.
	if len(planned) > 0 && (planned[0] == "/bin/sh" || planned[0] == "sh") {
		prefix = append(prefix, planned...)
	}
	return append(prefix, cmd...), nil
}

func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			if !replaced {
				out = append(out, prefix+val)
				replaced = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// freeLoopbackPort bind-probes 127.0.0.1:0 and hands the port back. The window
// between closing the listener and dsh binding it can lose the port to another
// process; that failure is logged instead of guessing a fixed port.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// procStartTime mirrors the backend's pid identity: the starttime field of
// /proc/<pid>/stat, published so the manager can reject a reused pid after a
// control-plane restart.
func procStartTime(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) <= 19 {
		return 0
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if status, ok := ee.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	return 1
}

func parseReadyToken(line string) (string, error) {
	const prefix = "dsh web:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("unexpected line %q", line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	token := u.Query().Get("token")
	if token == "" {
		return "", errors.New("ready line has no token")
	}
	return token, nil
}

func scan(r interface{ Read([]byte) (int, error) }, w *redactingWriter, ready chan<- string, wantReady bool) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if wantReady && strings.HasPrefix(line, "dsh web:") {
			select {
			case ready <- line:
			default:
			}
			fmt.Fprintln(w, "dsh startup line suppressed (contains token)")
			continue
		}
		fmt.Fprintln(w, line)
	}
}

type redactingWriter struct {
	mu sync.Mutex
	w  interface{ Write([]byte) (int, error) }
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := string(p)
	if strings.Contains(s, "token=") {
		s = "dsh output line suppressed (contains token)\n"
	}
	return r.w.Write([]byte(s))
}
