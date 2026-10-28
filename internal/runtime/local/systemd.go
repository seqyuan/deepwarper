package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/seqyuan/deepwarper/internal/runtime"
)

// tri-state for `runtime.local.systemdUser`.
const (
	systemdAuto   = "auto"
	systemdAlways = "always"
	systemdNever  = "never"
)

func normalizeTriState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return systemdAuto
	case "always", "true", "yes", "on", "force", "forced":
		return systemdAlways
	case "never", "false", "no", "off", "disable", "disabled":
		return systemdNever
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func normalizeLimiter(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "auto"
	}
	return s
}

// ResolveLimiter implements the configuration matrix:
//
//	auto      -> systemd if usable, else prlimit if present, else none
//	systemd   -> hard error when systemd is not usable (no silent downgrade)
//	prlimit   -> prlimit if present, else none
//	none      -> direct child
func ResolveLimiter(limiter, systemdUser string, systemdOK, prlimitOK bool) (string, error) {
	limiter = normalizeLimiter(limiter)
	tri := normalizeTriState(systemdUser)
	switch limiter {
	case "auto":
		if tri != systemdNever && systemdOK {
			return "systemd", nil
		}
		if prlimitOK {
			return "prlimit", nil
		}
		return "none", nil
	case "systemd":
		if tri == systemdNever {
			return "", errors.New("runtime.local.limiter=systemd contradicts systemdUser=never")
		}
		if !systemdOK {
			return "", errors.New("runtime.local.limiter=systemd but systemd --user is unavailable")
		}
		return "systemd", nil
	case "prlimit":
		if tri == systemdAlways {
			return "", errors.New("runtime.local.limiter=prlimit contradicts systemdUser=always")
		}
		if prlimitOK {
			return "prlimit", nil
		}
		return "none", nil
	case "none":
		if tri == systemdAlways {
			return "", errors.New("runtime.local.limiter=none contradicts systemdUser=always")
		}
		return "none", nil
	default:
		return "", fmt.Errorf("runtime.local.limiter %q is invalid (want auto|systemd|prlimit|none)", limiter)
	}
}

// Resolution is the effective, pre-start view used by both Start and
// `dw doctor`. Nothing here mutates the backend.
type Resolution struct {
	Limiter          string
	SystemdUser      string
	SystemdAvailable bool
	SystemdDetail    string
	PrlimitAvailable bool
	TasksMax         int
	StartTimeout     time.Duration
	StopGrace        time.Duration
	UnitStdio        string
}

// Resolve reports what Start would do on this host right now.
func (b *Backend) Resolve(ctx context.Context) (Resolution, error) {
	prlimitOK := b.prlimitAvailable()
	systemdOK, detail := b.systemdAvailability(ctx)
	limiter, err := ResolveLimiter(b.Limiter, b.SystemdUser, systemdOK, prlimitOK)
	res := Resolution{
		Limiter:          limiter,
		SystemdUser:      normalizeTriState(b.SystemdUser),
		SystemdAvailable: systemdOK,
		SystemdDetail:    detail,
		PrlimitAvailable: prlimitOK,
		TasksMax:         b.TasksMax,
		StartTimeout:     b.StartTimeout,
		StopGrace:        b.StopGrace,
		UnitStdio:        b.UnitStdio,
	}
	return res, err
}

func (b *Backend) prlimitAvailable() bool {
	_, err := b.runner().LookPath("prlimit")
	return err == nil
}

// systemdAvailability caches the probe per-backend (not per-package) so tests
// do not leak state into one another.
func (b *Backend) systemdAvailability(ctx context.Context) (bool, string) {
	tri := normalizeTriState(b.SystemdUser)
	switch tri {
	case systemdNever:
		return false, "disabled (systemdUser=never)"
	case systemdAlways:
		return true, "forced (systemdUser=always, probe skipped)"
	}
	b.mu.Lock()
	if b.systemdOK != nil {
		ok, detail := *b.systemdOK, b.systemdDetail
		b.mu.Unlock()
		return ok, detail
	}
	b.mu.Unlock()

	ok, detail := b.probeSystemd(ctx)
	b.mu.Lock()
	b.systemdOK = &ok
	b.systemdDetail = detail
	b.mu.Unlock()
	return ok, detail
}

func (b *Backend) probeSystemd(ctx context.Context) (bool, string) {
	r := b.runner()
	if _, err := r.LookPath("systemd-run"); err != nil {
		return false, "systemd-run not found in PATH"
	}
	if _, err := r.LookPath("systemctl"); err != nil {
		return false, "systemctl not found in PATH"
	}
	res, err := r.Run(ctx, "systemd-run", []string{"--user", "--scope", "--", "/bin/true"}, RunOptions{
		Timeout: 5 * time.Second,
		Env:     userManagerEnv(),
	})
	if err != nil {
		return false, "systemd-run --user --scope failed: " + err.Error()
	}
	if res.ExitCode != 0 {
		return false, fmt.Sprintf("systemd-run --user --scope exited %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return true, "systemd-run --user --scope ok"
}

// SystemdInfo is diagnostic-only; it is never used for a lifecycle decision.
type SystemdInfo struct {
	Available bool
	Detail    string
	Version   string
	Cgroup    string
	Linger    string
}

// Diagnose reports host-level facts for `dw doctor`. It ignores the
// configured tri-state so an operator can see reality even when they have
// forced systemd on or off.
func Diagnose(ctx context.Context, r Runner) SystemdInfo {
	if r == nil {
		r = osRunner{}
	}
	info := SystemdInfo{}
	if _, err := r.LookPath("systemd-run"); err != nil {
		info.Detail = "systemd-run not found in PATH"
	} else if _, err := r.LookPath("systemctl"); err != nil {
		info.Detail = "systemctl not found in PATH"
	} else if res, err := r.Run(ctx, "systemd-run", []string{"--user", "--scope", "--", "/bin/true"}, RunOptions{
		Timeout: 5 * time.Second,
		Env:     userManagerEnv(),
	}); err != nil {
		info.Detail = "systemd-run failed: " + err.Error()
	} else if res.ExitCode != 0 {
		info.Detail = fmt.Sprintf("systemd-run --user --scope exited %d: %s", res.ExitCode, firstLine(res.Stderr))
	} else {
		info.Available = true
		info.Detail = "systemd-run --user --scope ok"
	}
	if res, err := r.Run(ctx, "systemctl", []string{"--version"}, RunOptions{Timeout: 5 * time.Second, Env: []string{"LC_ALL=C"}}); err == nil && res.ExitCode == 0 {
		info.Version = firstLine(res.Stdout)
	}
	info.Cgroup = detectCgroupVersion()
	info.Linger = detectLinger(ctx, r)
	return info
}

func detectCgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "v2"
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			return "v2"
		}
	}
	return "v1"
}

func detectLinger(ctx context.Context, r Runner) string {
	if _, err := r.LookPath("loginctl"); err != nil {
		return "unknown (loginctl not found)"
	}
	uid := strconv.Itoa(os.Getuid())
	res, err := r.Run(ctx, "loginctl", []string{"show-user", uid, "-p", "Linger"}, RunOptions{Timeout: 5 * time.Second, Env: []string{"LC_ALL=C"}})
	if err != nil || res.ExitCode != 0 {
		return "unknown"
	}
	value := strings.TrimSpace(res.Stdout)
	if strings.HasSuffix(value, "Linger=yes") {
		return "yes"
	}
	if strings.HasSuffix(value, "Linger=no") {
		return "no (instances die on logout; run: loginctl enable-linger $USER)"
	}
	return "unknown"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

type unitStatus struct {
	Phase    runtime.Phase
	MainPID  int
	Result   string
	ExitCode int
	Detail   string
	Raw      map[string]string
}

func (s unitStatus) alive() bool { return s.Phase.Alive() }

// parseSystemctlShow parses `systemctl show -p ...` output. It never decides
// death from an empty response: that is unknown, and unknown is alive enough
// for callers not to delete records.
func parseSystemctlShow(stdout, stderr string) unitStatus {
	st := unitStatus{Phase: runtime.PhaseUnknown, Raw: map[string]string{}}
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		st.Raw[k] = v
	}
	if len(st.Raw) == 0 {
		if isGoneText(stderr) {
			st.Phase = runtime.PhaseNotFound
			st.Detail = firstLine(stderr)
		}
		return st
	}
	if v, err := strconv.Atoi(st.Raw["MainPID"]); err == nil {
		st.MainPID = v
	}
	if v, err := strconv.Atoi(st.Raw["ExecMainStatus"]); err == nil {
		st.ExitCode = v
	}
	st.Result = st.Raw["Result"]
	load := st.Raw["LoadState"]
	active := st.Raw["ActiveState"]
	if load == "not-found" || active == "not-found" {
		st.Phase = runtime.PhaseNotFound
	} else {
		switch active {
		case "active":
			st.Phase = runtime.PhaseActive
		case "activating":
			st.Phase = runtime.PhaseActivating
		case "reloading":
			st.Phase = runtime.PhaseReloading
		case "deactivating":
			st.Phase = runtime.PhaseDeactivating
		case "failed":
			st.Phase = runtime.PhaseFailed
		case "inactive":
			st.Phase = runtime.PhaseInactive
		default:
			st.Phase = runtime.PhaseUnknown
		}
	}
	if st.Phase == runtime.PhaseFailed || st.Phase == runtime.PhaseInactive {
		var bits []string
		if st.Result != "" {
			bits = append(bits, "result="+st.Result)
		}
		if st.ExitCode != 0 {
			bits = append(bits, "exit="+strconv.Itoa(st.ExitCode))
		}
		st.Detail = strings.Join(bits, " ")
	}
	return st
}

// isGoneText treats systemd's "unit does not exist" answers as success for
// stop and as not-found for probe. Everything else (DBus failure, permission
// error) must stay an error.
func isGoneText(s string) bool {
	s = strings.ToLower(s)
	for _, phrase := range []string{
		"not loaded", "not found", "no such unit", "could not be found", "does not exist",
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

func (b *Backend) runSystemctl(ctx context.Context, timeout time.Duration, args ...string) (RunResult, error) {
	return b.runner().Run(ctx, "systemctl", args, RunOptions{
		Timeout: timeout,
		Env:     append(userManagerEnv(), "LC_ALL=C", "LANG=C"),
	})
}

func (b *Backend) resetFailed(ctx context.Context, unit string) {
	// A previous failed run leaves the unit loaded, and systemd-run --unit
	// then refuses the name. Failure here is harmless (usually "not loaded").
	_, _ = b.runSystemctl(ctx, 5*time.Second, "--user", "reset-failed", unit)
}

func (b *Backend) unitStatus(ctx context.Context, unit string, timeout time.Duration) (unitStatus, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	res, err := b.runSystemctl(ctx, timeout,
		"--user", "show", unit,
		"-p", "LoadState",
		"-p", "ActiveState",
		"-p", "SubState",
		"-p", "MainPID",
		"-p", "Result",
		"-p", "ExecMainStatus",
	)
	if err != nil {
		return unitStatus{Phase: runtime.PhaseUnknown, Detail: err.Error()}, err
	}
	st := parseSystemctlShow(res.Stdout, res.Stderr)
	if res.ExitCode != 0 && st.Phase == runtime.PhaseUnknown && isGoneText(res.Stderr) {
		st.Phase = runtime.PhaseNotFound
		st.Detail = firstLine(res.Stderr)
	}
	if st.Phase == runtime.PhaseUnknown && res.ExitCode != 0 {
		return st, fmt.Errorf("systemctl show %s: exit %d: %s", unit, res.ExitCode, firstLine(res.Stderr))
	}
	return st, nil
}

// stopUnit stops a unit without a pre-probe. `systemctl stop` is the first
// command issued: a "does it exist?" pre-check with list-units silently
// answers false for a bare unit name and skips the stop entirely.
func (b *Backend) stopUnit(ctx context.Context, unit string, grace time.Duration) error {
	unit = normalizeUnitName(unit)
	if unit == "" {
		return nil
	}
	if grace <= 0 {
		grace = b.StopGrace
	}
	res, err := b.runSystemctl(ctx, grace+5*time.Second, "--user", "stop", unit)
	if err != nil {
		return fmt.Errorf("systemctl stop %s: %w", unit, err)
	}
	if res.ExitCode != 0 && !isGoneText(res.Stderr) {
		return fmt.Errorf("systemctl stop %s: exit %d: %s", unit, res.ExitCode, firstLine(res.Stderr))
	}

	deadline := time.Now().Add(grace)
	for {
		st, statusErr := b.unitStatus(ctx, unit, 5*time.Second)
		if statusErr == nil && !st.alive() {
			break
		}
		if time.Now().After(deadline) {
			kres, kerr := b.runSystemctl(ctx, 10*time.Second, "--user", "kill", "--signal=SIGKILL", unit)
			if kerr != nil {
				return fmt.Errorf("systemctl kill %s: %w", unit, kerr)
			}
			if kres.ExitCode != 0 && !isGoneText(kres.Stderr) {
				return fmt.Errorf("systemctl kill %s: exit %d: %s", unit, kres.ExitCode, firstLine(kres.Stderr))
			}
			st, statusErr = b.unitStatus(ctx, unit, 5*time.Second)
			if statusErr == nil && st.alive() {
				return fmt.Errorf("unit %s still active after SIGKILL", unit)
			}
			break
		}
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	// Free the name for the next start of the same instance.
	b.resetFailed(ctx, unit)
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// awaitUnitLoaded narrows the window in which an immediately following Probe
// could see "unknown": systemd-run returned, but the manager has not loaded
// the unit yet.
func (b *Backend) awaitUnitLoaded(ctx context.Context, unit string) unitStatus {
	budget := b.StartTimeout / 4
	if budget <= 0 || budget > 2*time.Second {
		budget = 2 * time.Second
	}
	deadline := time.Now().Add(budget)
	var st unitStatus
	for {
		var err error
		st, err = b.unitStatus(ctx, unit, 5*time.Second)
		if err == nil {
			switch st.Phase {
			case runtime.PhaseActive, runtime.PhaseActivating, runtime.PhaseReloading,
				runtime.PhaseFailed, runtime.PhaseInactive, runtime.PhaseNotFound:
				return st
			}
		}
		if time.Now().After(deadline) {
			return st
		}
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return st
		}
	}
}

// logPathVerdict reads the ExecStopPost verdict file. It is best-effort: the
// file only exists on hosts where the property was accepted.
func logPathVerdict(logPath string) string {
	if logPath == "" {
		return ""
	}
	data, err := os.ReadFile(logPath + ".verdict")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// appendLauncherFailure makes sure a systemd-run rejection is never recorded
// as "exit 1 over an empty log": the unit never existed, so systemd wrote
// nothing to the log.
func appendLauncherFailure(logPath, buffered string) {
	if logPath == "" {
		return
	}
	buffered = strings.TrimSpace(buffered)
	if buffered == "" {
		buffered = "(no launcher output)"
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[dw] launcher failed before the unit started:\n%s\n", buffered)
}
