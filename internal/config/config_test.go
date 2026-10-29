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
