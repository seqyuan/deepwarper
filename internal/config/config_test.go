package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadValidatesDistributionAndDefaults(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	data := []byte(`
server:
  dataDir: data
routing:
  mode: host
  hostSuffix: localhost
runtime:
  dshBin: /bin/true
distributions:
  default:
    path: dist
`)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.DefaultDistribution != "default" {
		t.Fatalf("default distribution: %q", c.Runtime.DefaultDistribution)
	}
	if c.Distributions["default"].ProfileName != "default" {
		t.Fatalf("profile name: %q", c.Distributions["default"].ProfileName)
	}
	if c.Server.DataDir != filepath.Join(root, "data") {
		t.Fatalf("data dir: %q", c.Server.DataDir)
	}
	if c.Auth.UsersDir != "" {
		t.Fatalf("usersDir: %q", c.Auth.UsersDir)
	}
	if c.Runtime.Moniforge != "" {
		t.Fatalf("moniforge: %q", c.Runtime.Moniforge)
	}
}

func TestLoadAuthAndMoniforge(t *testing.T) {
	path := writeConfig(t, `
server:
  dataDir: data
runtime:
  dshBin: /bin/true
  moniforge: mf
auth:
  usersDir: users
distributions:
  default:
    path: dist
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	if c.Auth.UsersDir != filepath.Join(root, "users") {
		t.Fatalf("usersDir: %q", c.Auth.UsersDir)
	}
	if c.Runtime.Moniforge != filepath.Join(root, "mf") {
		t.Fatalf("moniforge: %q", c.Runtime.Moniforge)
	}
}

func TestLoadRejectsMissingDistribution(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
runtime:
  dshBin: /bin/true
distributions:
  default:
    path: does-not-exist
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for missing distribution")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const baseConfig = `
server:
  dataDir: data
routing:
  mode: host
  hostSuffix: localhost
runtime:
  dshBin: /bin/true
%s
distributions:
  default:
    path: dist
`

func loadWithLocal(t *testing.T, local string) (*Config, error) {
	t.Helper()
	return Load(writeConfig(t, fmt.Sprintf(baseConfig, local)))
}

func TestLocalDefaults(t *testing.T) {
	c, err := loadWithLocal(t, "")
	if err != nil {
		t.Fatal(err)
	}
	l := c.Runtime.Local
	if l.Limiter != "auto" || l.SystemdUser != TriAuto {
		t.Fatalf("limiter/systemdUser: %q/%v", l.Limiter, l.SystemdUser)
	}
	if l.TasksMax != 512 {
		t.Fatalf("tasksMax: %d", l.TasksMax)
	}
	if c.SystemdStartTimeout() != 10*time.Second || c.StopGrace() != 20*time.Second {
		t.Fatalf("timeouts: %s/%s", c.SystemdStartTimeout(), c.StopGrace())
	}
	if l.UnitStdio != "log" || l.PrlimitCPUSeconds != 0 {
		t.Fatalf("unitStdio/prlimitCPUSeconds: %q/%d", l.UnitStdio, l.PrlimitCPUSeconds)
	}
}

func TestTriStateParsing(t *testing.T) {
	cases := map[string]TriState{
		"auto":   TriAuto,
		"always": TriAlways,
		"never":  TriNever,
		"true":   TriAlways,
		"false":  TriNever,
		"yes":    TriAlways,
		"no":     TriNever,
	}
	for in, want := range cases {
		var l LocalConfig
		if err := yaml.Unmarshal([]byte("systemdUser: "+in+"\n"), &l); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if l.SystemdUser != want {
			t.Fatalf("%s: got %v want %v", in, l.SystemdUser, want)
		}
	}
	var l LocalConfig
	if err := yaml.Unmarshal([]byte("systemdUser: true\n"), &l); err != nil {
		t.Fatal(err)
	}
	if l.SystemdUser != TriAlways {
		t.Fatalf("bool node: %v", l.SystemdUser)
	}
	if err := yaml.Unmarshal([]byte("systemdUser: sometimes\n"), &l); err == nil {
		t.Fatal("expected error for garbage tri-state")
	}
}

func TestLimiterContradictions(t *testing.T) {
	cases := []string{
		"  local:\n    limiter: systemd\n    systemdUser: never",
		"  local:\n    limiter: prlimit\n    systemdUser: always",
		"  local:\n    limiter: none\n    systemdUser: always",
		"  local:\n    limiter: bogus",
	}
	for _, local := range cases {
		if _, err := loadWithLocal(t, local); err == nil {
			t.Fatalf("expected error for:\n%s", local)
		}
	}
}

func TestLocalDurationValidation(t *testing.T) {
	cases := []string{
		"  local:\n    stopGrace: 1s",
		"  local:\n    systemdStartTimeout: 0s",
		"  local:\n    unitStdio: journald",
		"  local:\n    tasksMax: 10",
		"  local:\n    tasksMax: -1",
	}
	for _, local := range cases {
		if _, err := loadWithLocal(t, local); err == nil {
			t.Fatalf("expected error for:\n%s", local)
		}
	}
	// readyTimeout must exceed the launcher budget.
	body := fmt.Sprintf(baseConfig, "  local:\n    systemdStartTimeout: 10s")
	body = strings.Replace(body, "runtime:\n  dshBin: /bin/true", "runtime:\n  dshBin: /bin/true\n  readyTimeout: 5s", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("readyTimeout <= systemdStartTimeout must be rejected")
	}
}

func TestDataDirRejectsShellMetacharacters(t *testing.T) {
	body := strings.Replace(baseConfig, "dataDir: data", "dataDir: \"da'ta\"", 1)
	if _, err := Load(writeConfig(t, fmt.Sprintf(body, ""))); err == nil {
		t.Fatal("expected error for quote in dataDir")
	}
}

func TestUnknownDefaultBackendIsRejected(t *testing.T) {
	for _, name := range []string{"k8s", "sge", "pod"} {
		body := strings.Replace(fmt.Sprintf(baseConfig, ""), "runtime:\n", "runtime:\n  default: "+name+"\n", 1)
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatalf("runtime.default %s must fail at load", name)
		}
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"4Gi", 4 << 30},
		{"512Mi", 512 << 20},
		{"4G", 4 << 30},
		{"1024", 1024},
		{"1.5Gi", 3 << 29},
	}
	for _, c := range cases {
		got, err := ParseMemory(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %d want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "lots", "-1Gi"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func loadWithTop(t *testing.T, top string) (*Config, error) {
	t.Helper()
	body := strings.Replace(fmt.Sprintf(baseConfig, ""), "routing:\n", top+"routing:\n", 1)
	return Load(writeConfig(t, body))
}

func loadWithRouting(t *testing.T, routing string) (*Config, error) {
	t.Helper()
	body := strings.Replace(fmt.Sprintf(baseConfig, ""), "routing:\n  mode: host\n  hostSuffix: localhost\n", routing, 1)
	return Load(writeConfig(t, body))
}

func loadWithServer(t *testing.T, server string) (*Config, error) {
	t.Helper()
	body := strings.Replace(fmt.Sprintf(baseConfig, ""), "server:\n", "server:\n"+server, 1)
	return Load(writeConfig(t, body))
}

func TestDurationBoundsRejected(t *testing.T) {
	top := []string{
		"platform:\n  ticketTTL: 0s\n",
		"platform:\n  ticketTTL: -1s\n",
		"platform:\n  ticketTTL: 25h\n",
		"platform:\n  sessionTTL: 0s\n",
		"platform:\n  sessionTTL: 745h\n",
		"auth:\n  loginWindow: 0s\n",
		"auth:\n  loginLockout: 0s\n",
		"auth:\n  loginMaxAttempts: -1\n",
	}
	for _, body := range top {
		if _, err := loadWithTop(t, body); err == nil {
			t.Fatalf("expected error for:\n%s", body)
		}
	}
	local := []string{
		"  scanInterval: 0s",
		"  scanInterval: 500ms",
		"  scanInterval: 11m",
		"  heartbeat: 0s",
		"  heartbeat: 500ms",
		"  readyTimeout: 0s",
	}
	for _, body := range local {
		if _, err := loadWithLocal(t, body); err == nil {
			t.Fatalf("expected error for:\n%s", body)
		}
	}
}

func TestPortRangeValidation(t *testing.T) {
	for _, local := range []string{
		"  local:\n    portRange: [80, 3000]",
		"  local:\n    portRange: [20000, 20010]",
		"  local:\n    portRange: [30000, 20000]",
		"  local:\n    portRange: [40000, 70000]",
	} {
		if _, err := loadWithLocal(t, local); err == nil {
			t.Fatalf("expected error for:\n%s", local)
		}
	}
	if _, err := loadWithLocal(t, "  local:\n    portRange: [20000, 20015]"); err != nil {
		t.Fatalf("16-port range must be accepted: %v", err)
	}
}

func TestListenValidation(t *testing.T) {
	for _, listen := range []string{
		"  listen: 127.0.0.1:0\n",
		"  listen: 127.0.0.1:70000\n",
		"  listen: not-a-host-port\n",
		"  listen: '127.0.0.1:7658 bad'\n",
	} {
		if _, err := loadWithServer(t, listen); err == nil {
			t.Fatalf("expected error for:\n%s", listen)
		}
	}
}

func TestHostSuffixAndPublicRoutingValidation(t *testing.T) {
	bad := []string{
		"routing:\n  mode: host\n  hostSuffix: https://example.com\n",
		"routing:\n  mode: host\n  hostSuffix: example.com:8080\n",
		"routing:\n  mode: host\n  hostSuffix: -bad.example\n",
		"routing:\n  mode: host\n  hostSuffix: bad_.example\n",
		"routing:\n  mode: host\n  hostSuffix: .example.com\n",
		"routing:\n  mode: host\n  hostSuffix: example.com.\n",
		"routing:\n  mode: host\n  hostSuffix: Example.com\n",
		"routing:\n  mode: host\n  hostSuffix: localhost\n  publicScheme: ftp\n",
		"routing:\n  mode: host\n  hostSuffix: localhost\n  publicPort: -1\n",
		"routing:\n  mode: host\n  hostSuffix: localhost\n  publicPort: 70000\n",
	}
	for _, body := range bad {
		if _, err := loadWithRouting(t, body); err == nil {
			t.Fatalf("expected error for:\n%s", body)
		}
	}
	good := []string{
		"routing:\n  mode: host\n  hostSuffix: localhost\n",
		"routing:\n  mode: host\n  hostSuffix: a-b.example.com\n",
		"routing:\n  mode: host\n  hostSuffix: localhost\n  publicScheme: https\n  publicPort: 443\n  trustProxyHeaders: true\n",
	}
	for _, body := range good {
		if _, err := loadWithRouting(t, body); err != nil {
			t.Fatalf("expected success for:\n%s\n%v", body, err)
		}
	}
}

func TestDistributionKeyValidation(t *testing.T) {
	for _, key := range []string{"Default", "-lead", "with_underscore", "a.b"} {
		body := strings.Replace(fmt.Sprintf(baseConfig, ""), "  default:\n    path: dist", "  "+key+":\n    path: dist", 1)
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatalf("distribution key %q must be rejected", key)
		}
	}
}

func TestWorkspaceRootsValidation(t *testing.T) {
	if _, err := loadWithServer(t, "  workspaceRoots: [missing-root]\n"); err == nil {
		t.Fatal("a missing workspace root must be rejected")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWithServer(t, "  workspaceRoots: ["+file+"]\n"); err == nil {
		t.Fatal("a file workspace root must be rejected")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspaces"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	body := strings.Replace(fmt.Sprintf(baseConfig, ""), "server:\n", "server:\n  workspaceRoots: [workspaces]\n", 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("relative workspace root must resolve next to the config: %v", err)
	}
	if len(c.Server.WorkspaceRoots) != 1 || c.Server.WorkspaceRoots[0] != filepath.Join(root, "workspaces") {
		t.Fatalf("workspace roots: %#v", c.Server.WorkspaceRoots)
	}
}

func TestLoginLimitDefaultsAndOverrides(t *testing.T) {
	c, err := loadWithTop(t, "")
	if err != nil {
		t.Fatal(err)
	}
	max, window, lockout := c.LoginLimit()
	if max != 10 || window != 5*time.Minute || lockout != 5*time.Minute {
		t.Fatalf("login defaults: %d %s %s", max, window, lockout)
	}
	c, err = loadWithTop(t, "auth:\n  loginMaxAttempts: 3\n  loginWindow: 1m\n  loginLockout: 10m\n")
	if err != nil {
		t.Fatal(err)
	}
	max, window, lockout = c.LoginLimit()
	if max != 3 || window != time.Minute || lockout != 10*time.Minute {
		t.Fatalf("login overrides: %d %s %s", max, window, lockout)
	}
}

func TestEffectivePublicPort(t *testing.T) {
	c, err := loadWithServer(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.EffectivePublicPort(); got != 7658 {
		t.Fatalf("derived public port: %d", got)
	}
	c, err = loadWithTop(t, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Routing.PublicPort = 443
	if got := c.EffectivePublicPort(); got != 443 {
		t.Fatalf("explicit public port: %d", got)
	}
	var zero Config
	if got := zero.EffectivePublicPort(); got != 0 {
		t.Fatalf("zero config public port: %d", got)
	}
}

func TestZeroValueDurationAccessorsAreTotal(t *testing.T) {
	var c Config
	for name, got := range map[string]time.Duration{
		"ticket":       c.TicketTTL(),
		"session":      c.SessionTTL(),
		"systemdStart": c.SystemdStartTimeout(),
		"stopGrace":    c.StopGrace(),
		"readyTimeout": c.ReadyTimeout(),
		"scanInterval": c.ScanInterval(),
		"heartbeat":    c.Heartbeat(),
	} {
		if got <= 0 {
			t.Fatalf("%s duration must fall back to a default, got %s", name, got)
		}
	}
	max, window, lockout := c.LoginLimit()
	if max != defaultLoginMaxAttempts || window != defaultLoginWindow || lockout != defaultLoginLockout {
		t.Fatalf("zero config login limit: %d %s %s", max, window, lockout)
	}
}
