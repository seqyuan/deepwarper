package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Routing       RoutingConfig       `yaml:"routing"`
	Platform      PlatformConfig      `yaml:"platform"`
	Runtime       RuntimeConfig       `yaml:"runtime"`
	Quotas        QuotasConfig        `yaml:"quotas"`
	Distributions map[string]DistSpec `yaml:"distributions"`
}

type ServerConfig struct {
	Listen   string `yaml:"listen"`
	DataDir  string `yaml:"dataDir"`
	LogLevel string `yaml:"logLevel"`
}

type RoutingConfig struct {
	Mode         string `yaml:"mode"` // host | path
	HostSuffix   string `yaml:"hostSuffix"`
	PublicScheme string `yaml:"publicScheme"`
	PathBase     string `yaml:"pathBase"`
}

type PlatformConfig struct {
	Token       string `yaml:"token"`
	TokenFile   string `yaml:"tokenFile"`
	TicketTTL   string `yaml:"ticketTTL"`
	SessionTTL  string `yaml:"sessionTTL"`
	SessionFile string `yaml:"sessionFile"`
}

type RuntimeConfig struct {
	Default             string      `yaml:"default"`
	DefaultDistribution string      `yaml:"defaultDistribution"`
	DSHBin              string      `yaml:"dshBin"`
	DWBin               string      `yaml:"dwBin"`
	ReadyTimeout        string      `yaml:"readyTimeout"`
	ScanInterval        string      `yaml:"scanInterval"`
	Heartbeat           string      `yaml:"heartbeat"`
	Local               LocalConfig `yaml:"local"`
	// SGE is the cluster backend. Nil means "this site does not use a
	// scheduler"; the local backend is then the only one constructed.
	SGE *SGEConfig `yaml:"sge"`
}

// SGEConfig is the site's SGE/QSUB description. Every value is site-specific
// and comes from the config file: nothing about queues, paths or images is
// hardcoded in Go.
type SGEConfig struct {
	// Enabled is nil => enabled when the block is present.
	Enabled *bool `yaml:"enabled"`

	// Scheduler client binaries: PATH names or absolute paths. A site may use a
	// wrapper script that ssh's to a submit host, so these are never resolved at
	// config-load time.
	Qsub          string   `yaml:"qsub"`
	Qstat         string   `yaml:"qstat"`
	Qdel          string   `yaml:"qdel"`
	Qalter        string   `yaml:"qalter"`
	ExtraQsubArgs []string `yaml:"extraQsubArgs"`

	Queue          string `yaml:"queue"`          // -q; empty => cluster default
	PE             string `yaml:"pe"`             // -pe <pe> <slots>
	PEAccounting   string `yaml:"peAccounting"`   // cores | threads
	ThreadsPerCore int    `yaml:"threadsPerCore"` // threads mode only
	Project        string `yaml:"project"`        // -P
	// GPUFlag is the -l resource name used for GPU requests (default "gpu").
	// "none" disables the request entirely.
	GPUFlag string `yaml:"gpuResourceFlag"`

	// SubmitDir holds the job scripts; it must be on the shared filesystem.
	SubmitDir string `yaml:"submitDir"`
	// RendezvousDir is the shared filesystem root that must contain
	// <dataDir>/control, because the job writes there from a compute node.
	RendezvousDir string `yaml:"rendezvousDir"`

	QueueTimeout string `yaml:"queueTimeout"` // 0 = wait forever
	PollEvery    string `yaml:"pollEvery"`    // qstat + rendezvous cadence
	LaunchWait   string `yaml:"launchWait"`   // Ensure's blocking budget

	SSH                string   `yaml:"ssh"`
	SSHUser            string   `yaml:"sshUser"`
	SSHArgs            []string `yaml:"sshArgs"`
	Tunnel             *bool    `yaml:"tunnel"` // default true
	DirectDial         bool     `yaml:"directDial"`
	TunnelReadyTimeout string   `yaml:"tunnelReadyTimeout"`

	ApptainerImage string     `yaml:"apptainerImage"`
	ContainerArgs  []string   `yaml:"containerArgs"`
	HomeBox        string     `yaml:"homeBox"`
	WorkspaceBox   string     `yaml:"workspaceBox"`
	DistBox        string     `yaml:"distBox"`
	DWBox          string     `yaml:"dwBox"`
	DSHBox         string     `yaml:"dshBox"`
	ExtraBinds     []BindSpec `yaml:"extraBinds"`

	RenewBefore string `yaml:"renewBefore"`
	RenewFor    string `yaml:"renewFor"`
	WarnBefore  string `yaml:"warnBefore"`

	// IdleReap is off by default on SGE: the lifecycle ceiling is h_rt, and
	// killing a queued or preempted job forces a re-queue.
	IdleReap *bool `yaml:"idleReap"`

	// MinHVmemPerSlot floors the per-slot h_vmem request. h_vmem is enforced as
	// RLIMIT_AS, and a container runtime plus Node/V8 reserve a lot of virtual
	// address space even when RSS is small, so a literal CPU/memory split kills
	// the job with a confusing error.
	MinHVmemPerSlot string `yaml:"minHVmemPerSlot"`
}

// BindSpec is one extra --bind for the job's container: a host path mounted at
// an absolute path inside the box.
type BindSpec struct {
	Host     string `yaml:"host"`
	Box      string `yaml:"box"`
	ReadOnly bool   `yaml:"readOnly"`
}

// IsEnabled reports whether the sge backend should be constructed.
func (s *SGEConfig) IsEnabled() bool {
	if s == nil {
		return false
	}
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// TunnelEnabled defaults to true: without it the endpoint names a compute node
// and the gateway (which only dials loopback) cannot reach it.
func (s *SGEConfig) TunnelEnabled() bool {
	if s == nil || s.Tunnel == nil {
		return true
	}
	return *s.Tunnel
}

// IdleReapEnabled defaults to false on SGE (see SGEConfig.IdleReap).
func (s *SGEConfig) IdleReapEnabled() bool {
	if s == nil || s.IdleReap == nil {
		return false
	}
	return *s.IdleReap
}

// ControlRoot is the per-instance control directory root. It is the existing
// dataDir/control, not a second rendezvous format; sge.rendezvousDir must
// contain it so a compute node writes where the login node reads.
func (s *SGEConfig) ControlRoot(dataDir string) string {
	return filepath.Join(dataDir, "control")
}

type LocalConfig struct {
	PortRange [2]int `yaml:"portRange"`
	// Limiter is auto|systemd|prlimit|none. Empty means auto; existing
	// configs that say `prlimit` keep taking the exact prlimit path.
	Limiter             string   `yaml:"limiter"`
	SystemdUser         TriState `yaml:"systemdUser"`
	TasksMax            int      `yaml:"tasksMax"`
	SystemdStartTimeout string   `yaml:"systemdStartTimeout"`
	StopGrace           string   `yaml:"stopGrace"`
	UnitStdio           string   `yaml:"unitStdio"`
	// PrlimitCPUSeconds is a cumulative CPU-time ceiling. 0 (default) means
	// do not set RLIMIT_CPU: for a long-lived service CPU*3600 kills a
	// healthy instance after a few CPU-hours.
	PrlimitCPUSeconds int      `yaml:"prlimitCPUSeconds"`
	DshArgs           []string `yaml:"dshArgs"`
	ExtraArgs         []string `yaml:"extraArgs"`
}

// TriState is auto|always|never. It also accepts YAML booleans so
// `systemdUser: true` keeps reading naturally.
type TriState int

const (
	TriAuto TriState = iota
	TriAlways
	TriNever
)

func (t TriState) String() string {
	switch t {
	case TriAlways:
		return "always"
	case TriNever:
		return "never"
	default:
		return "auto"
	}
}

func (t *TriState) UnmarshalYAML(n *yaml.Node) error {
	if n == nil {
		*t = TriAuto
		return nil
	}
	raw := strings.ToLower(strings.TrimSpace(n.Value))
	if n.Tag == "!!bool" {
		if raw == "true" || raw == "yes" || raw == "on" {
			*t = TriAlways
			return nil
		}
		if raw == "false" || raw == "no" || raw == "off" {
			*t = TriNever
			return nil
		}
	}
	switch raw {
	case "", "auto":
		*t = TriAuto
	case "always", "true", "yes", "on", "force":
		*t = TriAlways
	case "never", "false", "no", "off", "disable":
		*t = TriNever
	default:
		return fmt.Errorf("systemdUser must be auto, always or never (got %q)", n.Value)
	}
	return nil
}

func (t TriState) MarshalYAML() (any, error) { return t.String(), nil }

type QuotasConfig struct {
	MaxInstancesPerUser int    `yaml:"maxInstancesPerUser"`
	MaxCPUPerUser       int    `yaml:"maxCPUPerUser"`
	MaxMemoryPerUser    string `yaml:"maxMemoryPerUser"`
	MaxWalltime         string `yaml:"maxWalltime"`
}

type DistSpec struct {
	Name        string       `yaml:"name"`
	Version     string       `yaml:"version"`
	ProfileName string       `yaml:"profileName"`
	Path        string       `yaml:"path"`
	Defaults    DistDefaults `yaml:"defaults"`
}

type DistDefaults struct {
	CPU      int    `yaml:"cpu"`
	Memory   string `yaml:"memory"`
	Walltime string `yaml:"walltime"`
	IdleTTL  string `yaml:"idleTTL"`
	GPU      int    `yaml:"gpu"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults(path)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults(path string) {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:7658"
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = "./dw-data"
	}
	if c.Routing.Mode == "" {
		c.Routing.Mode = "host"
	}
	if c.Routing.PublicScheme == "" {
		c.Routing.PublicScheme = "http"
	}
	if c.Routing.HostSuffix == "" {
		c.Routing.HostSuffix = "localhost"
	}
	if c.Platform.TicketTTL == "" {
		c.Platform.TicketTTL = "60s"
	}
	if c.Platform.SessionTTL == "" {
		c.Platform.SessionTTL = "12h"
	}
	if c.Runtime.Default == "" {
		c.Runtime.Default = "local"
	}
	if c.Runtime.DefaultDistribution == "" {
		c.Runtime.DefaultDistribution = "default"
	}
	if c.Runtime.ReadyTimeout == "" {
		c.Runtime.ReadyTimeout = "90s"
	}
	if c.Runtime.ScanInterval == "" {
		c.Runtime.ScanInterval = "30s"
	}
	if c.Runtime.Heartbeat == "" {
		c.Runtime.Heartbeat = "30s"
	}
	if c.Runtime.Local.PortRange == [2]int{} {
		c.Runtime.Local.PortRange = [2]int{20000, 30000}
	}
	if c.Runtime.Local.Limiter == "" {
		c.Runtime.Local.Limiter = "auto"
	}
	if c.Runtime.Local.TasksMax == 0 {
		c.Runtime.Local.TasksMax = 512
	}
	if c.Runtime.Local.SystemdStartTimeout == "" {
		c.Runtime.Local.SystemdStartTimeout = "10s"
	}
	if c.Runtime.Local.StopGrace == "" {
		c.Runtime.Local.StopGrace = "20s"
	}
	if c.Runtime.Local.UnitStdio == "" {
		c.Runtime.Local.UnitStdio = "log"
	}
	if c.Quotas.MaxInstancesPerUser == 0 {
		c.Quotas.MaxInstancesPerUser = 1
	}
	c.applySGEDefaults()
	abs, err := filepath.Abs(path)
	if err == nil {
		c.Server.DataDir = absJoin(filepath.Dir(abs), c.Server.DataDir)
		if c.Platform.TokenFile != "" {
			c.Platform.TokenFile = absJoin(filepath.Dir(abs), c.Platform.TokenFile)
		}
		if c.Platform.SessionFile == "" {
			c.Platform.SessionFile = filepath.Join(c.Server.DataDir, "secrets", "session_hmac")
		} else {
			c.Platform.SessionFile = absJoin(filepath.Dir(abs), c.Platform.SessionFile)
		}
		for name, d := range c.Distributions {
			d.Path = absJoin(filepath.Dir(abs), d.Path)
			c.Distributions[name] = d
		}
		// Shared-filesystem paths are resolved against the config file like
		// dataDir is; the image path is not, because it must exist on a compute
		// node, where the config file does not.
		if s := c.Runtime.SGE; s != nil {
			s.SubmitDir = absJoin(filepath.Dir(abs), s.SubmitDir)
			s.RendezvousDir = absJoin(filepath.Dir(abs), s.RendezvousDir)
		}
	}
}

func absJoin(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(base, p))
}

// applySGEDefaults fills the scheduler defaults. It runs relative to the config
// file directory for the same reason dataDir does: a site writes
// `submitDir: ../sge` meaning "next to this file".
func (c *Config) applySGEDefaults() {
	s := c.Runtime.SGE
	if s == nil {
		return
	}
	if s.Qsub == "" {
		s.Qsub = "qsub"
	}
	if s.Qstat == "" {
		s.Qstat = "qstat"
	}
	if s.Qdel == "" {
		s.Qdel = "qdel"
	}
	if s.Qalter == "" {
		s.Qalter = "qalter"
	}
	if s.SSH == "" {
		s.SSH = "ssh"
	}
	if s.PEAccounting == "" {
		s.PEAccounting = "cores"
	}
	if s.GPUFlag == "" {
		s.GPUFlag = "gpu"
	}
	if s.PollEvery == "" {
		s.PollEvery = "2s"
	}
	if s.QueueTimeout == "" {
		s.QueueTimeout = "0s"
	}
	if s.LaunchWait == "" {
		s.LaunchWait = "0s"
	}
	if s.TunnelReadyTimeout == "" {
		s.TunnelReadyTimeout = "30s"
	}
	if s.WarnBefore == "" {
		s.WarnBefore = "30m"
	}
	if s.MinHVmemPerSlot == "" {
		s.MinHVmemPerSlot = "4Gi"
	}
	if len(s.ContainerArgs) == 0 {
		// --containall gives a private /tmp and /dev (dsh needs a writable
		// tmp), --cleanenv keeps the login node's environment out of the job,
		// --no-home stops apptainer from mounting the real home over our box.
		s.ContainerArgs = []string{"--containall", "--cleanenv", "--no-home"}
	}
	if s.HomeBox == "" {
		s.HomeBox = "/home/dsh"
	}
	if s.WorkspaceBox == "" {
		s.WorkspaceBox = "/workspace"
	}
	if s.DistBox == "" {
		s.DistBox = "/opt/dw/dist"
	}
	if s.DWBox == "" {
		s.DWBox = "/usr/local/bin/dw"
	}
	if s.DSHBox == "" {
		s.DSHBox = "/opt/dw/bin/dsh"
	}
}

func (c *Config) Validate() error {
	var problems []string
	bad := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	if c.Routing.Mode != "host" && c.Routing.Mode != "path" {
		bad("routing.mode must be host or path")
	}
	if c.Routing.Mode == "path" {
		bad("routing.mode path is not implemented in this build; use host")
	}
	if c.Routing.Mode == "host" && c.Routing.HostSuffix == "" {
		bad("routing.hostSuffix is required in host mode")
	}
	if c.Server.Listen == "" {
		bad("server.listen is required")
	}
	if c.Runtime.Local.PortRange[0] <= 0 || c.Runtime.Local.PortRange[1] < c.Runtime.Local.PortRange[0] {
		bad("runtime.local.portRange is invalid")
	}
	if c.Runtime.DSHBin == "" {
		bad("runtime.dshBin is required")
	}
	if _, err := time.ParseDuration(c.Platform.TicketTTL); err != nil {
		bad("platform.ticketTTL: %v", err)
	}
	if _, err := time.ParseDuration(c.Platform.SessionTTL); err != nil {
		bad("platform.sessionTTL: %v", err)
	}
	if _, err := time.ParseDuration(c.Runtime.ReadyTimeout); err != nil {
		bad("runtime.readyTimeout: %v", err)
	}
	if _, err := time.ParseDuration(c.Runtime.ScanInterval); err != nil {
		bad("runtime.scanInterval: %v", err)
	}
	if err := c.validateLocal(); err != nil {
		problems = append(problems, err.Error())
	}
	if err := c.validateSGE(); err != nil {
		problems = append(problems, err.Error())
	}
	if err := validateUnitPath("server.dataDir", c.Server.DataDir); err != nil {
		bad("%v", err)
	}
	for name, d := range c.Distributions {
		if d.Name == "" {
			d.Name = name
		}
		if d.ProfileName == "" {
			d.ProfileName = d.Name
		}
		if d.Path == "" {
			bad("distribution %q: path is required", name)
		}
		if _, err := os.Stat(d.Path); err != nil {
			bad("distribution %q: %v", name, err)
		}
		if strings.ContainsAny(d.ProfileName, "/\\") || d.ProfileName == "." || d.ProfileName == ".." {
			bad("distribution %q: invalid profileName", name)
		}
		c.Distributions[name] = d
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (c *Config) TicketTTL() time.Duration {
	d, _ := time.ParseDuration(c.Platform.TicketTTL)
	return d
}

func (c *Config) SessionTTL() time.Duration {
	d, _ := time.ParseDuration(c.Platform.SessionTTL)
	return d
}

func (c *Config) SystemdStartTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Runtime.Local.SystemdStartTimeout)
	return d
}

func (c *Config) StopGrace() time.Duration {
	d, _ := time.ParseDuration(c.Runtime.Local.StopGrace)
	return d
}

func (c *Config) ReadyTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Runtime.ReadyTimeout)
	return d
}

func (c *Config) ScanInterval() time.Duration {
	d, _ := time.ParseDuration(c.Runtime.ScanInterval)
	return d
}

func (c *Config) Heartbeat() time.Duration {
	d, _ := time.ParseDuration(c.Runtime.Heartbeat)
	return d
}

// validateLocal enforces the limiter/systemdUser matrix and duration bounds.
// Contradictions are rejected rather than silently resolved: a config that
// demands cgroups must not quietly run without them.
func (c *Config) validateLocal() error {
	l := c.Runtime.Local
	var problems []string
	bad := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	limiter := strings.ToLower(strings.TrimSpace(l.Limiter))
	switch limiter {
	case "auto", "systemd", "prlimit", "none":
	default:
		bad("runtime.local.limiter %q is invalid (want auto|systemd|prlimit|none)", l.Limiter)
	}
	systemdUser := l.SystemdUser.String()
	if limiter == "systemd" && systemdUser == "never" {
		bad("runtime.local.limiter=systemd contradicts runtime.local.systemdUser=never")
	}
	if limiter == "prlimit" && systemdUser == "always" {
		bad("runtime.local.limiter=prlimit contradicts runtime.local.systemdUser=always")
	}
	if limiter == "none" && systemdUser == "always" {
		bad("runtime.local.limiter=none contradicts runtime.local.systemdUser=always")
	}
	if l.TasksMax != 0 && (l.TasksMax < 32 || l.TasksMax > 65536) {
		bad("runtime.local.tasksMax must be 0 or between 32 and 65536")
	}
	start, err := time.ParseDuration(l.SystemdStartTimeout)
	if err != nil || start <= 0 {
		bad("runtime.local.systemdStartTimeout must be a positive duration")
	}
	grace, err := time.ParseDuration(l.StopGrace)
	if err != nil || grace <= 0 {
		bad("runtime.local.stopGrace must be a positive duration")
	} else if grace < 5*time.Second {
		bad("runtime.local.stopGrace must be at least 5s")
	}
	if ready, readyErr := time.ParseDuration(c.Runtime.ReadyTimeout); readyErr == nil {
		if start > 0 && ready <= start {
			bad("runtime.readyTimeout (%s) must be greater than runtime.local.systemdStartTimeout (%s)", ready, start)
		}
	}
	if l.UnitStdio != "" && l.UnitStdio != "log" && l.UnitStdio != "journal" {
		bad("runtime.local.unitStdio must be log or journal")
	}
	if l.PrlimitCPUSeconds < 0 {
		bad("runtime.local.prlimitCPUSeconds must be >= 0")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func validateUnitPath(label, p string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsAny(p, "'\"\n\r%") {
		return fmt.Errorf("%s %q contains one of ' \" newline %% which cannot be used in a systemd unit property", label, p)
	}
	return nil
}

// validateSGE collects every problem in the sge section at once. It never
// touches the filesystem: whether /data/dw/images/dw-runtime.sif exists on a
// *compute* node is a `dw doctor` probe, not a login-node config error.
func (c *Config) validateSGE() error {
	s := c.Runtime.SGE
	defaultName := strings.ToLower(strings.TrimSpace(c.Runtime.Default))
	if defaultName != "" && defaultName != "local" && defaultName != "sge" {
		return fmt.Errorf("runtime.default %q is not a known backend (want local|sge)", c.Runtime.Default)
	}
	if s == nil || !s.IsEnabled() {
		if defaultName == "sge" {
			return fmt.Errorf("runtime.default is sge but runtime.sge is absent or disabled")
		}
		return nil
	}
	var problems []string
	bad := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if s.SubmitDir == "" {
		bad("runtime.sge.submitDir is required (job scripts must land on a shared filesystem)")
	} else if !filepath.IsAbs(s.SubmitDir) {
		bad("runtime.sge.submitDir must be an absolute path (got %q)", s.SubmitDir)
	}
	if s.RendezvousDir == "" {
		bad("runtime.sge.rendezvousDir is required (the job writes its state there)")
	} else if !filepath.IsAbs(s.RendezvousDir) {
		bad("runtime.sge.rendezvousDir must be an absolute path (got %q)", s.RendezvousDir)
	} else if root := s.ControlRoot(c.Server.DataDir); root != "" && !isBeneath(s.RendezvousDir, root) {
		bad("runtime.sge.rendezvousDir (%s) must contain server.dataDir/control (%s): otherwise the job writes where the login node does not read",
			s.RendezvousDir, root)
	}
	if strings.TrimSpace(s.PE) == "" {
		bad("runtime.sge.pe is required (the parallel environment name, e.g. smp)")
	}
	switch strings.ToLower(strings.TrimSpace(s.PEAccounting)) {
	case "", "cores":
	case "threads":
		if s.ThreadsPerCore < 1 {
			bad("runtime.sge.threadsPerCore must be >= 1 when peAccounting is threads")
		}
	default:
		bad("runtime.sge.peAccounting must be cores|threads (got %q)", s.PEAccounting)
	}
	if s.TunnelEnabled() && s.DirectDial {
		bad("runtime.sge.tunnel=true contradicts runtime.sge.directDial=true")
	}
	if !s.TunnelEnabled() && !s.DirectDial {
		bad("runtime.sge.tunnel=false requires directDial=true: the gateway only dials loopback")
	}
	if s.ApptainerImage == "" {
		bad("runtime.sge.apptainerImage is required")
	} else if !filepath.IsAbs(s.ApptainerImage) {
		bad("runtime.sge.apptainerImage must be an absolute path on the compute node (got %q)", s.ApptainerImage)
	}
	for _, box := range []struct{ name, path string }{
		{"homeBox", s.HomeBox},
		{"workspaceBox", s.WorkspaceBox},
		{"distBox", s.DistBox},
		{"dwBox", s.DWBox},
		{"dshBox", s.DSHBox},
	} {
		if box.path != "" && !filepath.IsAbs(box.path) {
			bad("runtime.sge.%s must be an absolute in-container path (got %q)", box.name, box.path)
		}
	}
	if s.HomeBox != "" && s.HomeBox == s.WorkspaceBox {
		bad("runtime.sge.homeBox and runtime.sge.workspaceBox must differ")
	}
	for i, b := range s.ExtraBinds {
		if !filepath.IsAbs(b.Host) || !filepath.IsAbs(b.Box) {
			bad("runtime.sge.extraBinds[%d]: host and box must be absolute paths", i)
		}
		if b.Host == "/" || b.Box == "/" {
			bad("runtime.sge.extraBinds[%d]: refusing to bind / (mount the concrete path)", i)
		}
	}

	poll, pollErr := time.ParseDuration(strings.TrimSpace(s.PollEvery))
	if pollErr != nil {
		bad("runtime.sge.pollEvery: %v", pollErr)
	} else if poll > 0 && poll < 500*time.Millisecond {
		bad("runtime.sge.pollEvery must be at least 500ms (got %s)", poll)
	}
	for _, d := range []struct{ name, value string }{
		{"queueTimeout", s.QueueTimeout},
		{"launchWait", s.LaunchWait},
		{"tunnelReadyTimeout", s.TunnelReadyTimeout},
		{"renewBefore", s.RenewBefore},
		{"renewFor", s.RenewFor},
		{"warnBefore", s.WarnBefore},
	} {
		if _, err := parseSGEDuration(d.value); err != nil {
			bad("runtime.sge.%s: %v", d.name, err)
		}
	}
	renewBefore := sgeDurationOf(s.RenewBefore)
	renewFor := sgeDurationOf(s.RenewFor)
	warnBefore := sgeDurationOf(s.WarnBefore)
	if renewFor > 0 && renewBefore <= 0 {
		bad("runtime.sge.renewFor is set but renewBefore is zero, so renewal would never fire")
	}
	if renewBefore > 0 && warnBefore > 0 && warnBefore < renewBefore {
		bad("runtime.sge.warnBefore (%s) is smaller than renewBefore (%s), so the warning would never fire", warnBefore, renewBefore)
	}
	if _, err := ParseMemory(s.MinHVmemPerSlot); err != nil {
		bad("runtime.sge.minHVmemPerSlot: %v", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// sgeDuration reads an optional duration: empty means zero (the feature is off).
func sgeDurationOf(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
}

// parseSGEuration validates an optional duration with a useful message.
func parseSGEDuration(s string) (time.Duration, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", s)
	}
	return d, nil
}

// isBeneath reports whether path is dir itself or lives inside it.
func isBeneath(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if dir == path {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// ParseMemory parses the memory strings DeepWarper configs use: "4Gi", "512Mi",
// "4G", "1024". It is the config/backend spelling of a request; the local
// backend keeps its own byte-compatible parser for prlimit argv.
func ParseMemory(s string) (uint64, error) {
	raw := strings.TrimSpace(strings.ToUpper(s))
	if raw == "" {
		return 0, fmt.Errorf("empty memory")
	}
	mult := uint64(1)
	for _, suf := range []struct {
		s string
		m uint64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GI", 1 << 30}, {"MI", 1 << 20}, {"KI", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	} {
		if strings.HasSuffix(raw, suf.s) {
			mult = suf.m
			raw = strings.TrimSuffix(raw, suf.s)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid memory %q: negative", s)
	}
	return uint64(n * float64(mult)), nil
}
