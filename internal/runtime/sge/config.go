// Package sge is the SGE/qsub runtime backend.
//
// On a cluster an instance is a *job*, not a process. That single fact drives
// everything here:
//
//   - Resources are enforced by SGE, not by this process. The instance's
//     declaration becomes `-pe`, `-l h_vmem`, `-l h_rt` and `-q`; there is no
//     cgroup layer, and adding one would fight the scheduler.
//   - The lifecycle ceiling is `h_rt`, not a timer here. Killing a job early to
//     re-queue it is user-hostile, so idle reaping is off by default.
//   - There is no reliable inbound path from the scheduler to a login node, so
//     the *shared filesystem is the control channel* and an `ssh -L` local
//     forward is the data channel. The job writes what it became; the login node
//     reads it.
//   - A suspended job is not a dead job. SGE preempts with SIGSTOP (`s` state)
//     and may resume; conflating that with failure would tear down work that is
//     merely paused.
//
// Every scheduler command goes through the Runner seam, and every tunnel through
// the TunnelFactory seam, so the whole backend is testable without a cluster.
package sge

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seqyuan/deepwarper/internal/config"
)

// Runner executes external commands. Tests replace it; production uses
// ExecRunner.
type Runner interface {
	// Run executes and returns combined stdout+stderr.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the production Runner.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// PortAllocator hands out the loopback port the tunnel listens on. The existing
// internal/ports.Pool implements it, so a tunnel cannot collide with a local
// instance's port.
type PortAllocator interface {
	Acquire(owner string) (int, error)
	Release(port int)
}

// DialFunc reports whether a loopback port accepts connections. It is a seam so
// tests can decide when a tunnel "works" without spawning ssh.
type DialFunc func(host string, port int, timeout time.Duration) bool

// Bind is one container bind: a host path mounted at an absolute in-container
// path.
type Bind struct {
	Host     string
	Box      string
	ReadOnly bool
}

// Flag renders the --bind value.
func (b Bind) Flag() string {
	mode := "rw"
	if b.ReadOnly {
		mode = "ro"
	}
	return b.Host + ":" + b.Box + ":" + mode
}

// Config is the typed backend configuration. It is the only thing the backend
// works with, which is what lets the tests build one directly.
type Config struct {
	Qsub, Qstat, Qdel, Qalter string
	ExtraQsubArgs             []string

	Queue          string
	PE             string
	PEAccounting   string
	ThreadsPerCore int
	Project        string
	GPUFlag        string

	// SubmitDir holds the job scripts (shared filesystem).
	SubmitDir string
	// RendezvousDir is the shared filesystem root that must contain ControlRoot.
	RendezvousDir string
	// ControlRoot is the existing <dataDir>/control, bound into the container at
	// its own path so no rendezvous path needs translating.
	ControlRoot string

	QueueTimeout time.Duration
	PollEvery    time.Duration
	LaunchWait   time.Duration
	// ReadyWait bounds the wait for ready.json once the job is running.
	ReadyWait time.Duration

	SSH                string
	SSHUser            string
	SSHArgs            []string
	Tunnel             bool
	DirectDial         bool
	TunnelReadyTimeout time.Duration

	ApptainerImage string
	ContainerArgs  []string
	HomeBox        string
	WorkspaceBox   string
	DistBox        string
	DWBox          string
	DSHBox         string
	ExtraBinds     []Bind

	RenewBefore time.Duration
	RenewFor    time.Duration
	WarnBefore  time.Duration

	IdleReap        bool
	MinHVmemPerSlot uint64
	// RetryDelay spaces tunnel rebuild attempts after a control-plane restart.
	RetryDelay time.Duration

	// Ports is the login-node port pool. Nil means "no tunnel possible".
	Ports PortAllocator
	// Runner is nil in production (ExecRunner).
	Runner Runner
	// NewTunnel is nil in production (real `ssh -L`).
	NewTunnel TunnelFactory
	// Now is a test seam.
	Now func() time.Time
	// Dialable is a test seam for "is this loopback port usable".
	Dialable DialFunc
	// ReadCmdline and KillPID are seams for the stale-tunnel sweep; nil means
	// /proc/<pid>/cmdline and syscall.Kill.
	ReadCmdline func(pid int) (string, error)
	KillPID     func(pid int, sig syscall.Signal) error
}

// Backend implements runtime.Backend for SGE. The job lifecycle lives in
// backend.go, readiness and adoption in endpoint.go, renewal in renew.go.
type Backend struct {
	Config

	mu      sync.Mutex
	jobs    []Job
	jobsAt  time.Time
	jobsErr error
	misses  map[string]int
	tunnels map[string]TunnelRuntime
}

// FromConfig translates the site config into a backend. It is the only place
// the config schema and the backend meet.
func FromConfig(c *config.Config) (*Backend, error) {
	s := c.Runtime.SGE
	if s == nil || !s.IsEnabled() {
		return nil, errors.New("runtime.sge is not configured")
	}
	cfg := Config{
		Qsub:          s.Qsub,
		Qstat:         s.Qstat,
		Qdel:          s.Qdel,
		Qalter:        s.Qalter,
		ExtraQsubArgs: s.ExtraQsubArgs,

		Queue:          s.Queue,
		PE:             s.PE,
		PEAccounting:   strings.ToLower(strings.TrimSpace(s.PEAccounting)),
		ThreadsPerCore: s.ThreadsPerCore,
		Project:        s.Project,
		GPUFlag:        s.GPUFlag,

		SubmitDir:     s.SubmitDir,
		RendezvousDir: s.RendezvousDir,
		ControlRoot:   s.ControlRoot(c.Server.DataDir),

		SSH:                s.SSH,
		SSHUser:            s.SSHUser,
		SSHArgs:            s.SSHArgs,
		Tunnel:             s.TunnelEnabled(),
		DirectDial:         s.DirectDial,
		TunnelReadyTimeout: time.Second,

		ApptainerImage: s.ApptainerImage,
		ContainerArgs:  s.ContainerArgs,
		HomeBox:        s.HomeBox,
		WorkspaceBox:   s.WorkspaceBox,
		DistBox:        s.DistBox,
		DWBox:          s.DWBox,
		DSHBox:         s.DSHBox,

		IdleReap: s.IdleReapEnabled(),
	}
	for _, b := range s.ExtraBinds {
		cfg.ExtraBinds = append(cfg.ExtraBinds, Bind{Host: b.Host, Box: b.Box, ReadOnly: b.ReadOnly})
	}
	for _, d := range []struct {
		dst    *time.Duration
		value  string
		label  string
		minVal time.Duration
	}{
		{&cfg.QueueTimeout, s.QueueTimeout, "queueTimeout", 0},
		{&cfg.PollEvery, s.PollEvery, "pollEvery", 0},
		{&cfg.LaunchWait, s.LaunchWait, "launchWait", 0},
		{&cfg.TunnelReadyTimeout, s.TunnelReadyTimeout, "tunnelReadyTimeout", time.Second},
		{&cfg.ReadyWait, c.Runtime.ReadyTimeout, "runtime.readyTimeout", time.Second},
		{&cfg.RenewBefore, s.RenewBefore, "renewBefore", 0},
		{&cfg.RenewFor, s.RenewFor, "renewFor", 0},
		{&cfg.WarnBefore, s.WarnBefore, "warnBefore", 0},
	} {
		parsed, err := parseOptionalDuration(d.value)
		if err != nil {
			return nil, fmt.Errorf("runtime.sge.%s: %w", d.label, err)
		}
		if parsed > 0 && parsed < d.minVal {
			return nil, fmt.Errorf("runtime.sge.%s: %s is too small", d.label, parsed)
		}
		*d.dst = parsed
	}
	minHVmem, err := config.ParseMemory(s.MinHVmemPerSlot)
	if err != nil {
		return nil, fmt.Errorf("runtime.sge.minHVmemPerSlot: %w", err)
	}
	cfg.MinHVmemPerSlot = minHVmem

	b := &Backend{Config: cfg}
	b.applyDefaults()
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// SetPorts wires the shared login-node port pool. The caller that owns the pool
// does this, so the backend can be built before the pool exists.
func (b *Backend) SetPorts(p PortAllocator) { b.Ports = p }

func (b *Backend) applyDefaults() {
	for _, d := range []struct {
		dst   *string
		value string
	}{
		{&b.Qsub, "qsub"},
		{&b.Qstat, "qstat"},
		{&b.Qdel, "qdel"},
		{&b.Qalter, "qalter"},
		{&b.SSH, "ssh"},
		{&b.GPUFlag, "gpu"},
		{&b.HomeBox, "/home/dsh"},
		{&b.WorkspaceBox, "/workspace"},
		{&b.DistBox, "/opt/dw/dist"},
		{&b.DWBox, "/usr/local/bin/dw"},
		{&b.DSHBox, "/opt/dw/bin/dsh"},
	} {
		if strings.TrimSpace(*d.dst) == "" {
			*d.dst = d.value
		}
	}
	if len(b.ContainerArgs) == 0 {
		b.ContainerArgs = []string{"--containall", "--cleanenv", "--no-home"}
	}
	if b.PollEvery <= 0 {
		b.PollEvery = 2 * time.Second
	}
	if b.TunnelReadyTimeout <= 0 {
		b.TunnelReadyTimeout = 30 * time.Second
	}
	if b.ReadyWait <= 0 {
		b.ReadyWait = 90 * time.Second
	}
	if b.RetryDelay <= 0 {
		b.RetryDelay = 2 * time.Second
	}
	if b.MinHVmemPerSlot == 0 {
		b.MinHVmemPerSlot = 4 << 30
	}
	if b.PEAccounting == "" {
		b.PEAccounting = "cores"
	}
	if b.misses == nil {
		b.misses = map[string]int{}
	}
	if b.tunnels == nil {
		b.tunnels = map[string]TunnelRuntime{}
	}
}

func (b *Backend) Name() string { return "sge" }

// Validate checks the configuration before any job is submitted. It is cheap
// and is re-run by Start, so a config change since load cannot slip through.
func (b *Backend) Validate() error {
	var problems []string
	bad := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	if strings.TrimSpace(b.SubmitDir) == "" || !filepath.IsAbs(b.SubmitDir) {
		bad("submitDir is required and must be absolute (job scripts must land on a shared filesystem)")
	}
	if strings.TrimSpace(b.RendezvousDir) == "" || !filepath.IsAbs(b.RendezvousDir) {
		bad("rendezvousDir is required and must be absolute (it is the only control channel)")
	}
	if b.ControlRoot != "" && b.RendezvousDir != "" && !isBeneath(b.RendezvousDir, b.ControlRoot) {
		bad("rendezvousDir (%s) must contain the control root (%s)", b.RendezvousDir, b.ControlRoot)
	}
	if strings.TrimSpace(b.PE) == "" {
		bad("pe is required (a service needs at least one slot; SGE rejects an unknown -pe)")
	}
	switch b.PEAccounting {
	case "", "cores":
	case "threads":
		if b.ThreadsPerCore < 1 {
			bad("threadsPerCore must be >= 1 when peAccounting is threads")
		}
	default:
		bad("peAccounting must be cores|threads (got %q)", b.PEAccounting)
	}
	if b.Tunnel && b.DirectDial {
		bad("tunnel and directDial are mutually exclusive")
	}
	if !b.Tunnel && !b.DirectDial {
		bad("tunnel=false requires directDial=true: the gateway only dials loopback")
	}
	if strings.TrimSpace(b.ApptainerImage) == "" || !filepath.IsAbs(b.ApptainerImage) {
		bad("apptainerImage is required and must be an absolute path on the compute node")
	}
	if b.RenewFor > 0 && b.RenewBefore <= 0 {
		bad("renewFor is set but renewBefore is zero (renewal would never fire)")
	}
	if b.RenewBefore > 0 && b.WarnBefore > 0 && b.WarnBefore < b.RenewBefore {
		bad("warnBefore (%s) is smaller than renewBefore (%s), so the warning would never fire", b.WarnBefore, b.RenewBefore)
	}
	if b.PollEvery > 0 && b.PollEvery < 500*time.Millisecond {
		bad("pollEvery must be at least 500ms")
	}
	if b.HomeBox == b.WorkspaceBox {
		bad("homeBox and workspaceBox must differ")
	}
	if len(problems) > 0 {
		return fmt.Errorf("sge backend: %s", strings.Join(problems, "; "))
	}
	return nil
}

func isBeneath(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if dir == path {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

func (b *Backend) runner() Runner {
	if b.Runner != nil {
		return b.Runner
	}
	return ExecRunner{}
}

func (b *Backend) bin(name string) string {
	switch name {
	case "qsub":
		if b.Qsub != "" {
			return b.Qsub
		}
	case "qstat":
		if b.Qstat != "" {
			return b.Qstat
		}
	case "qdel":
		if b.Qdel != "" {
			return b.Qdel
		}
	case "qalter":
		if b.Qalter != "" {
			return b.Qalter
		}
	}
	return name
}

func (b *Backend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Backend) dialable(port int, timeout time.Duration) bool {
	if b.Dialable != nil {
		return b.Dialable("127.0.0.1", port, timeout)
	}
	return dialTCP(port, timeout)
}

// slotsFor maps a CPU request onto SGE slots. Sites differ on whether a slot is
// a core or a thread, so the site declares the accounting mode.
func (b *Backend) slotsFor(cpu int) int {
	slots := cpu
	if slots < 1 {
		slots = 1
	}
	if b.PEAccounting == "threads" && b.ThreadsPerCore > 0 {
		slots *= b.ThreadsPerCore
	}
	return slots
}

// parseOptionalDuration reads a Go duration. Empty is zero.
func parseOptionalDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (want a Go duration like 10m or 1h30m)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", s)
	}
	return d, nil
}

// firstLine is what a user sees of a scheduler error: the rest is usually a
// stack of advice that hides the cause.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
