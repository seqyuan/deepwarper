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
	Auth          AuthConfig          `yaml:"auth"`
}

// AuthConfig turns on the login page when UsersDir is set. An empty
// directory keeps the historical ticket-only gateway.
type AuthConfig struct {
	UsersDir string `yaml:"usersDir"`
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
	Default             string `yaml:"default"`
	DefaultDistribution string `yaml:"defaultDistribution"`
	DSHBin              string `yaml:"dshBin"`
	DWBin               string `yaml:"dwBin"`
	ReadyTimeout        string `yaml:"readyTimeout"`
	ScanInterval        string `yaml:"scanInterval"`
	Heartbeat           string `yaml:"heartbeat"`
	// Moniforge is a shared conda/mamba prefix bind-mounted read-only.
	// Empty means conda is not mounted.
	Moniforge string      `yaml:"moniforge"`
	Local     LocalConfig `yaml:"local"`
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
		if c.Auth.UsersDir != "" {
			c.Auth.UsersDir = absJoin(filepath.Dir(abs), c.Auth.UsersDir)
		}
		if c.Runtime.Moniforge != "" {
			c.Runtime.Moniforge = absJoin(filepath.Dir(abs), c.Runtime.Moniforge)
		}
	}
}

func absJoin(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(base, p))
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
	defaultName := strings.ToLower(strings.TrimSpace(c.Runtime.Default))
	if defaultName != "" && defaultName != "local" {
		bad("runtime.default %q is not supported; this build runs a local process per user", c.Runtime.Default)
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
