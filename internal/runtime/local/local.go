package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

type modeKind int

const (
	modeProcess modeKind = iota
	modeUnit
)

// Options configures the local backend. Zero values are production defaults,
// so `&Backend{DwBin: ...}` keeps working for simple callers.
type Options struct {
	DwBin             string
	Limiter           string // auto | systemd | prlimit | none
	SystemdUser       string // auto | always | never
	ReadyWait         time.Duration
	TasksMax          int
	StartTimeout      time.Duration
	StopGrace         time.Duration
	UnitStdio         string // log | journal
	PrlimitCPUSeconds int
	Run               Runner
}

type Backend struct {
	Options

	mu             sync.Mutex
	systemdOK      *bool
	systemdDetail  string
	negativeProbes map[string]int
}

// New returns a backend with defaults applied. Backend methods remain usable
// on a zero-value Backend, but New is the normal constructor.
func New(opts Options) *Backend {
	b := &Backend{Options: opts}
	b.applyDefaults()
	return b
}

func (b *Backend) applyDefaults() {
	if b.ReadyWait <= 0 {
		b.ReadyWait = 90 * time.Second
	}
	if b.TasksMax <= 0 {
		b.TasksMax = 512
	}
	if b.StartTimeout <= 0 {
		b.StartTimeout = 10 * time.Second
	}
	if b.StopGrace <= 0 {
		b.StopGrace = 20 * time.Second
	}
	if b.UnitStdio == "" {
		b.UnitStdio = "log"
	}
	b.Limiter = normalizeLimiter(b.Limiter)
	b.SystemdUser = normalizeTriState(b.SystemdUser)
	if b.negativeProbes == nil {
		b.negativeProbes = map[string]int{}
	}
}

func (b *Backend) Name() string { return "local" }

func (b *Backend) runner() Runner {
	if b.Run != nil {
		return b.Run
	}
	return osRunner{}
}

func (b *Backend) Start(ctx context.Context, spec agent.Spec) (runtime.Handle, error) {
	if b.DwBin == "" {
		return nil, errors.New("local backend: dw binary is required")
	}
	b.applyDefaults()
	// bwrap wraps dsh inside the agent so this process can still see the
	// control directory. Fail here when the spec asks for it and it is
	// not installed, before a unit or child is created.
	if err := b.ensureBwrap(spec); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.ControlDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return nil, err
	}
	if err := rendezvous.Clear(spec.ControlDir); err != nil {
		return nil, err
	}
	specData, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := rendezvous.Write(spec.ControlDir, "spec.json", specData, 0o600); err != nil {
		return nil, err
	}

	limiter, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if limiter == "systemd" {
		return b.startUnit(ctx, spec)
	}
	return b.startChild(ctx, spec, limiter)
}

func (b *Backend) ensureBwrap(spec agent.Spec) error {
	if len(spec.BwrapArgv) == 0 {
		return nil
	}
	name := strings.TrimSpace(spec.BwrapArgv[0])
	if name == "" {
		name = "bwrap"
	}
	if _, err := b.runner().LookPath(name); err != nil {
		return fmt.Errorf("bwrap requested but not found: %w", err)
	}
	return nil
}

func (b *Backend) resolve(ctx context.Context) (string, error) {
	systemdOK, detail := b.systemdAvailability(ctx)
	limiter, err := ResolveLimiter(b.Limiter, b.SystemdUser, systemdOK, b.prlimitAvailable())
	if err != nil {
		if detail != "" {
			return "", fmt.Errorf("%w (%s)", err, detail)
		}
		return "", err
	}
	return limiter, nil
}

func (b *Backend) startUnit(ctx context.Context, spec agent.Spec) (*Handle, error) {
	unit := normalizeUnitName(spec.UnitName)
	if err := validateUnitName(unit); err != nil {
		return nil, err
	}
	if err := validateUnitPropertyPath("working directory", spec.Workspace); err != nil {
		return nil, err
	}
	if err := validateUnitPropertyPath("log path", spec.LogPath); err != nil {
		return nil, err
	}
	if err := validateUnitPropertyPath("home", spec.Home); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return nil, err
	}
	// Pre-create the log with 0600 so systemd's `append:` inherits the
	// restrictive mode instead of creating a 0644 file. The fd is closed:
	// the unit does not inherit our descriptors.
	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_ = logFile.Close()
	// A stale verdict must never be read as this run's outcome.
	_ = os.Remove(spec.LogPath + ".verdict")

	// A previous failed run leaves the unit loaded and systemd-run --unit
	// then refuses the name.
	b.resetFailed(ctx, unit)

	systemdRun, err := b.runner().LookPath("systemd-run")
	if err != nil || systemdRun == "" {
		systemdRun = "systemd-run"
	}
	argv, err := unitArgv(UnitArgvOptions{
		SystemdRun:       systemdRun,
		UnitName:         unit,
		Description:      "deepwarper " + spec.InstanceID,
		CPU:              spec.CPU,
		Memory:           spec.Memory,
		TasksMax:         b.TasksMax,
		StopGrace:        b.StopGrace,
		WorkingDirectory: spec.Workspace,
		LogPath:          spec.LogPath,
		UnitStdio:        b.UnitStdio,
		PATH:             unitPath(spec.DshBin),
		Home:             spec.Home,
		DwBin:            b.DwBin,
		SpecPath:         specPath(spec.ControlDir),
	})
	if err != nil {
		return nil, err
	}

	res, runErr := b.runner().Run(ctx, argv[0], argv[1:], RunOptions{
		Timeout: b.StartTimeout,
		Env:     userManagerEnv(),
	})
	if runErr != nil || res.ExitCode != 0 {
		buffered := res.Stderr
		if runErr != nil {
			buffered = strings.TrimSpace(buffered + "\n" + runErr.Error())
		}
		appendLauncherFailure(spec.LogPath, buffered)
		msg := firstLine(res.Stderr)
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("systemd-run exited %d", res.ExitCode)
		}
		return nil, fmt.Errorf("systemd-run %s: %s", unit, msg)
	}

	st := b.awaitUnitLoaded(ctx, unit)
	return &Handle{
		backend:     b,
		mode:        modeUnit,
		unit:        unit,
		controlDir:  spec.ControlDir,
		incarnation: spec.Incarnation,
		logPath:     spec.LogPath,
		command:     append([]string{}, argv...),
		limiter:     "systemd",
		pid:         st.MainPID,
		done:        make(chan struct{}),
	}, nil
}

// unitPath removes the dependency on systemd-run forwarding the client
// environment: dsh is a shebang script resolved through PATH.
func unitPath(dshBin string) string {
	base := os.Getenv("PATH")
	if base == "" {
		base = "/usr/local/bin:/usr/bin:/bin"
	}
	if dshBin == "" {
		return base
	}
	return filepath.Dir(dshBin) + ":" + base
}

// Probe implements runtime.Backend. Unit mode only reports a confirmed death
// after three consecutive negative readings, so a transient systemctl hiccup
// cannot orphan a live instance.
func (b *Backend) Probe(ctx context.Context, inst *model.Instance) (runtime.Observation, error) {
	if inst == nil {
		return runtime.Observation{Phase: runtime.PhaseUnknown}, nil
	}
	ref := normalizeUnitName(inst.BackendRef)
	if ref == "" {
		return b.probePID(inst), nil
	}
	obs, err := b.probeUnitRaw(ctx, ref, inst.LogPath)
	if err != nil {
		return obs, err
	}
	switch obs.Phase {
	case runtime.PhaseFailed, runtime.PhaseInactive, runtime.PhaseNotFound:
		n := b.bumpNegative(ref)
		obs.NegativeReadings = n
		if n < 3 {
			prefix := fmt.Sprintf("pending gone confirmation (%d/3)", n)
			if obs.Detail != "" {
				prefix += ": " + obs.Detail
			}
			obs.Detail = prefix
			obs.Phase = runtime.PhaseUnknown
		}
	default:
		b.clearNegative(ref)
	}
	return obs, nil
}

func (b *Backend) probePID(inst *model.Instance) runtime.Observation {
	obs := runtime.Observation{
		Source:   "pid",
		PID:      inst.PID,
		PIDStart: inst.PIDStart,
	}
	if inst.PID <= 0 {
		obs.Phase = runtime.PhaseUnknown
		obs.Detail = "no pid recorded"
		return obs
	}
	if !processAlive(inst.PID, 0) {
		obs.Phase = runtime.PhaseInactive
		obs.Detail = "process not found"
		return obs
	}
	if inst.PIDStart > 0 {
		if pidStartTime(inst.PID) != inst.PIDStart {
			obs.Phase = runtime.PhaseInactive
			obs.Detail = "pid was reused"
			return obs
		}
	}
	obs.Phase = runtime.PhaseActive
	return obs
}

func (b *Backend) probeUnitRaw(ctx context.Context, unit, logPath string) (runtime.Observation, error) {
	st, err := b.unitStatus(ctx, unit, 5*time.Second)
	obs := runtime.Observation{
		Phase:    st.Phase,
		Source:   "systemd",
		Ref:      unit,
		PID:      st.MainPID,
		ExitCode: st.ExitCode,
		Detail:   st.Detail,
	}
	if st.Phase.Gone() {
		obs.Verdict = logPathVerdict(logPath)
	}
	if err != nil {
		obs.Phase = runtime.PhaseUnknown
		if obs.Detail == "" {
			obs.Detail = err.Error()
		}
		return obs, err
	}
	return obs, nil
}

func (b *Backend) bumpNegative(unit string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.negativeProbes == nil {
		b.negativeProbes = map[string]int{}
	}
	b.negativeProbes[unit]++
	return b.negativeProbes[unit]
}

func (b *Backend) clearNegative(unit string) {
	b.mu.Lock()
	delete(b.negativeProbes, unit)
	b.mu.Unlock()
}

// Stop implements runtime.Backend. It works with no in-memory handle and no
// pre-probe, which is what makes `dw serve` restarts stop correctly.
func (b *Backend) Stop(ctx context.Context, inst *model.Instance) error {
	if inst == nil {
		return nil
	}
	if ref := normalizeUnitName(inst.BackendRef); ref != "" {
		return b.stopUnit(ctx, ref, b.StopGrace)
	}
	return stopRecordedProcess(ctx, inst.PID, inst.PIDStart, specPath(inst.ControlDir))
}

// Handle is the in-process view of a started instance.
type Handle struct {
	backend     *Backend
	mode        modeKind
	unit        string
	cmd         *exec.Cmd
	controlDir  string
	incarnation string
	logPath     string
	command     []string
	limiter     string
	pid         int
	pidStart    uint64
	done        chan struct{}
	waitErr     error

	stopOnce sync.Once
	stopErr  error
}

func (h *Handle) Ref() string {
	if h.mode == modeUnit {
		return h.unit
	}
	if h.pid > 0 {
		return strconv.Itoa(h.pid)
	}
	return ""
}

func (h *Handle) PID() int { return h.pid }

func (h *Handle) PIDStart() uint64 {
	if h.mode == modeUnit {
		return 0
	}
	return h.pidStart
}
func (h *Handle) Limiter() string { return h.limiter }
func (h *Handle) Command() []string {
	return append([]string{}, h.command...)
}
func (h *Handle) LogPath() string { return h.logPath }

func (h *Handle) Alive() bool {
	if h.mode == modeUnit {
		obs, err := h.backend.probeUnitRaw(context.Background(), h.unit, h.logPath)
		if err != nil {
			return false
		}
		return obs.Alive()
	}
	select {
	case <-h.done:
		return false
	default:
	}
	if h.cmd == nil || h.cmd.Process == nil {
		return false
	}
	return processAlive(h.cmd.Process.Pid, h.pidStart)
}

// Endpoint waits for ready.json. Unit mode additionally watches the unit, so
// an OOM-kill that no agent observed fails fast with systemd's verdict
// instead of burning the full ready timeout.
func (h *Handle) Endpoint(ctx context.Context, timeout time.Duration) (*rendezvous.Ready, error) {
	if timeout <= 0 {
		timeout = h.backend.ReadyWait
	}
	if h.mode != modeUnit {
		return rendezvous.WaitReady(ctx, h.controlDir, h.incarnation, timeout)
	}
	var mu sync.Mutex
	var last time.Time
	var gone bool
	var message string
	probe := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if gone {
			return message, true
		}
		// ready.json is polled at 200ms; spawning systemctl at that rate
		// would be 5 forks/s for no information.
		if time.Since(last) < 2*time.Second {
			return "", false
		}
		last = time.Now()
		obs, err := h.backend.probeUnitRaw(ctx, h.unit, h.logPath)
		if err != nil {
			return "", false
		}
		if obs.Gone() {
			gone = true
			message = obs.Describe()
			return message, true
		}
		return "", false
	}
	return rendezvous.WaitReadyProbe(ctx, h.controlDir, h.incarnation, timeout, probe)
}

func (h *Handle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		if h.mode == modeUnit {
			h.stopErr = h.backend.stopUnit(ctx, h.unit, h.backend.StopGrace)
			return
		}
		h.stopErr = h.stopChild(ctx)
	})
	return h.stopErr
}
