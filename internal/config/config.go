package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/deepwarper/internal/model"
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
	// LoginMaxAttempts, LoginWindow and LoginLockout bound the per-IP login
	// rate limiter. Zero or empty values fall back to the defaults.
	LoginMaxAttempts int    `yaml:"loginMaxAttempts"`
	LoginWindow      string `yaml:"loginWindow"`
	LoginLockout     string `yaml:"loginLockout"`
}

type ServerConfig struct {
	Listen   string `yaml:"listen"`
	DataDir  string `yaml:"dataDir"`
	LogLevel string `yaml:"logLevel"`
	// WorkspaceRoots are extra directories a caller may request as a
	// workspace. Relative entries are resolved next to the config file.
	WorkspaceRoots []string `yaml:"workspaceRoots"`
}

type RoutingConfig struct {
	Mode         string `yaml:"mode"` // host | path
	HostSuffix   string `yaml:"hostSuffix"`
	PublicScheme string `yaml:"publicScheme"`
	PathBase     string `yaml:"pathBase"`
	// PublicPort is the externally visible port. 0 derives it from listen.
	PublicPort int `yaml:"publicPort"`
	// TrustProxyHeaders accepts X-Forwarded-Host/-Proto from the fronting
	// proxy when set.
	TrustProxyHeaders bool `yaml:"trustProxyHeaders"`
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
	if c.Auth.LoginMaxAttempts == 0 {
		c.Auth.LoginMaxAttempts = defaultLoginMaxAttempts
	}
	if c.Auth.LoginWindow == "" {
		c.Auth.LoginWindow = "5m"
	}
	if c.Auth.LoginLockout == "" {
		c.Auth.LoginLockout = "5m"
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		c.Server.DataDir = absJoin(filepath.Dir(abs), c.Server.DataDir)
		for i, root := range c.Server.WorkspaceRoots {
			c.Server.WorkspaceRoots[i] = absJoin(filepath.Dir(abs), root)
		}
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
	if c.Routing.HostSuffix != "" {
		if err := validateHostSuffix(c.Routing.HostSuffix); err != nil {
			bad("routing.hostSuffix: %v", err)
		}
	}
	switch c.Routing.PublicScheme {
	case "http", "https":
	default:
		bad("routing.publicScheme must be http or https")
	}
	if c.Routing.PublicPort < 0 || c.Routing.PublicPort > 65535 {
		bad("routing.publicPort must be between 0 and 65535")
	}
	if c.Server.Listen == "" {
		bad("server.listen is required")
	} else if err := validateListen(c.Server.Listen); err != nil {
		bad("server.listen: %v", err)
	}
	lo, hi := c.Runtime.Local.PortRange[0], c.Runtime.Local.PortRange[1]
	if lo < 1024 || hi > 65535 || hi < lo {
		bad("runtime.local.portRange must be within 1024-65535 with high >= low")
	} else if hi-lo+1 < 16 {
		bad("runtime.local.portRange must contain at least 16 ports")
	}
	if c.Runtime.DSHBin == "" {
		bad("runtime.dshBin is required")
	}
	positiveDuration := func(label, raw string, min, max time.Duration) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			bad("%s: %v", label, err)
			return
		}
		if d <= 0 {
			bad("%s must be a positive duration", label)
			return
		}
		if min > 0 && d < min {
			bad("%s must be at least %s", label, min)
		}
		if max > 0 && d > max {
			bad("%s must be at most %s", label, max)
		}
	}
	positiveDuration("platform.ticketTTL", c.Platform.TicketTTL, 0, 24*time.Hour)
	positiveDuration("platform.sessionTTL", c.Platform.SessionTTL, 0, 30*24*time.Hour)
	positiveDuration("runtime.readyTimeout", c.Runtime.ReadyTimeout, 0, 0)
	positiveDuration("runtime.scanInterval", c.Runtime.ScanInterval, time.Second, 10*time.Minute)
	positiveDuration("runtime.heartbeat", c.Runtime.Heartbeat, time.Second, 0)
	positiveDuration("auth.loginWindow", c.Auth.LoginWindow, 0, 0)
	positiveDuration("auth.loginLockout", c.Auth.LoginLockout, 0, 0)
	if c.Auth.LoginMaxAttempts < 0 {
		bad("auth.loginMaxAttempts must be >= 0")
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
	for _, root := range c.Server.WorkspaceRoots {
		if strings.TrimSpace(root) == "" {
			bad("server.workspaceRoots contains an empty path")
			continue
		}
		if !filepath.IsAbs(root) {
			bad("server.workspaceRoots %q must be absolute", root)
		}
		if strings.ContainsAny(root, "\x00\r\n") {
			bad("server.workspaceRoots %q contains a control character", root)
			continue
		}
		fi, err := os.Stat(root)
		if err != nil {
			bad("server.workspaceRoots %q: %v", root, err)
			continue
		}
		if !fi.IsDir() {
			bad("server.workspaceRoots %q is not a directory", root)
		}
	}
	for name, d := range c.Distributions {
		if err := model.ValidateDistKey(name); err != nil {
			bad("distribution %q: %v", name, err)
		}
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
		if !model.IsSafeFileComponent(d.Name) {
			bad("distribution %q: invalid name", name)
		}
		if !model.IsSafeFileComponent(d.ProfileName) {
			bad("distribution %q: invalid profileName", name)
		}
		c.Distributions[name] = d
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

var (
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

func validateListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if strings.ContainsAny(host, " \t\r\n\x00") {
		return fmt.Errorf("host %q contains whitespace or a control character", host)
	}
	return nil
}

func validateHostSuffix(suffix string) error {
	if len(suffix) > 253 {
		return fmt.Errorf("name is longer than 253 bytes")
	}
	if strings.ContainsAny(suffix, "/\\:@ \t\r\n\x00") {
		return fmt.Errorf("%q must be a bare DNS name", suffix)
	}
	if strings.HasPrefix(suffix, ".") || strings.HasSuffix(suffix, ".") {
		return fmt.Errorf("%q must not start or end with a dot", suffix)
	}
	for _, label := range strings.Split(suffix, ".") {
		if len(label) == 0 || len(label) > 63 || !hostLabelRE.MatchString(label) {
			return fmt.Errorf("%q is not a valid DNS name", suffix)
		}
	}
	return nil
}

const (
	defaultTicketTTL           = 60 * time.Second
	defaultSessionTTL          = 12 * time.Hour
	defaultSystemdStartTimeout = 10 * time.Second
	defaultStopGrace           = 20 * time.Second
	defaultReadyTimeout        = 90 * time.Second
	defaultScanInterval        = 30 * time.Second
	defaultHeartbeat           = 30 * time.Second
	defaultLoginMaxAttempts    = 10
	defaultLoginWindow         = 5 * time.Minute
	defaultLoginLockout        = 5 * time.Minute
)

// positiveDurationOr returns def when raw is missing, unparsable or
// non-positive so a zero-value Config never trips a ticker or timer.
func positiveDurationOr(raw string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func (c *Config) TicketTTL() time.Duration {
	return positiveDurationOr(c.Platform.TicketTTL, defaultTicketTTL)
}

func (c *Config) SessionTTL() time.Duration {
	return positiveDurationOr(c.Platform.SessionTTL, defaultSessionTTL)
}

func (c *Config) SystemdStartTimeout() time.Duration {
	return positiveDurationOr(c.Runtime.Local.SystemdStartTimeout, defaultSystemdStartTimeout)
}

func (c *Config) StopGrace() time.Duration {
	return positiveDurationOr(c.Runtime.Local.StopGrace, defaultStopGrace)
}

func (c *Config) ReadyTimeout() time.Duration {
	return positiveDurationOr(c.Runtime.ReadyTimeout, defaultReadyTimeout)
}

func (c *Config) ScanInterval() time.Duration {
	return positiveDurationOr(c.Runtime.ScanInterval, defaultScanInterval)
}

func (c *Config) Heartbeat() time.Duration {
	return positiveDurationOr(c.Runtime.Heartbeat, defaultHeartbeat)
}

// EffectivePublicPort is the port users see: routing.publicPort when set,
// otherwise the server.listen port.
func (c *Config) EffectivePublicPort() int {
	if c.Routing.PublicPort > 0 {
		return c.Routing.PublicPort
	}
	_, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

// LoginLimit returns the effective login rate-limit settings. Missing or
// invalid values fall back to the documented defaults.
func (c *Config) LoginLimit() (int, time.Duration, time.Duration) {
	max := c.Auth.LoginMaxAttempts
	if max <= 0 {
		max = defaultLoginMaxAttempts
	}
	window := positiveDurationOr(c.Auth.LoginWindow, defaultLoginWindow)
	lockout := positiveDurationOr(c.Auth.LoginLockout, defaultLoginLockout)
	return max, window, lockout
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
