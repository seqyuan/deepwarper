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

// sgeConfig is a minimal valid SGE section (paths lexical: Validate never stats
// the shared filesystem, only `dw doctor` does).
const validSGE = `
  sge:
    pe: smp
    submitDir: /data/dw/sge/submit
    rendezvousDir: /data/dw
    apptainerImage: /data/dw/images/dw-runtime.sif
`

func loadSGE(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	body := fmt.Sprintf(baseConfig, extra)
	body = strings.Replace(body, "  dataDir: data", "  dataDir: /data/dw", 1)
	return Load(writeConfig(t, body))
}

func TestSGEDefaults(t *testing.T) {
	c, err := loadSGE(t, validSGE)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Runtime.SGE
	if s == nil {
		t.Fatal("sge section missing")
	}
	if !s.IsEnabled() || !s.TunnelEnabled() || s.IdleReapEnabled() {
		t.Fatalf("defaults: enabled=%v tunnel=%v idleReap=%v", s.IsEnabled(), s.TunnelEnabled(), s.IdleReapEnabled())
	}
	for name, got := range map[string]string{
		"qsub": s.Qsub, "qstat": s.Qstat, "qdel": s.Qdel, "qalter": s.Qalter, "ssh": s.SSH,
		"peAccounting": s.PEAccounting, "gpuResourceFlag": s.GPUFlag, "pollEvery": s.PollEvery,
		"tunnelReadyTimeout": s.TunnelReadyTimeout, "warnBefore": s.WarnBefore,
		"minHVmemPerSlot": s.MinHVmemPerSlot, "homeBox": s.HomeBox,
		"workspaceBox": s.WorkspaceBox, "distBox": s.DistBox, "dwBox": s.DWBox, "dshBox": s.DSHBox,
	} {
		if got == "" {
			t.Fatalf("%s default is missing", name)
		}
	}
	if len(s.ContainerArgs) == 0 {
		t.Fatal("containerArgs default is missing")
	}
}

func TestSGERejectsContradictions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "renewFor without renewBefore",
			body: validSGE + "    renewFor: 2h\n",
			want: "renewBefore",
		},
		{
			name: "warnBefore below renewBefore",
			body: validSGE + "    renewBefore: 1h\n    warnBefore: 10m\n",
			want: "warnBefore",
		},
		{
			name: "tunnel and directDial",
			body: validSGE + "    directDial: true\n",
			want: "directDial",
		},
		{
			name: "no tunnel and no directDial",
			body: validSGE + "    tunnel: false\n",
			want: "tunnel=false",
		},
		{
			name: "control root outside the shared filesystem",
			body: strings.Replace(validSGE, "rendezvousDir: /data/dw", "rendezvousDir: /scratch/rdv", 1),
			want: "must contain",
		},
		{
			name: "missing pe",
			body: strings.Replace(validSGE, "    pe: smp\n", "", 1),
			want: "pe is required",
		},
		{
			name: "threads without threadsPerCore",
			body: validSGE + "    peAccounting: threads\n",
			want: "threadsPerCore",
		},
		{
			name: "relative image",
			body: strings.Replace(validSGE, "apptainerImage: /data/dw/images/dw-runtime.sif", "apptainerImage: images/dw.sif", 1),
			want: "apptainerImage",
		},
		{
			name: "bind of root",
			body: validSGE + "    extraBinds:\n      - host: /\n        box: /data\n",
			want: "extraBinds",
		},
		{
			name: "pollEvery too small",
			body: validSGE + "    pollEvery: 10ms\n",
			want: "pollEvery",
		},
		{
			name: "bad memory",
			body: validSGE + "    minHVmemPerSlot: lots\n",
			want: "minHVmemPerSlot",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadSGE(t, c.body)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error must mention %q: %v", c.want, err)
			}
		})
	}
}

// TestSGERelativePathResolvesAgainstTheConfigFile documents the one place a
// relative SGE path is legitimate: submitDir next to the config file.
func TestSGERelativePathResolvesAgainstTheConfigFile(t *testing.T) {
	body := strings.Replace(validSGE, "submitDir: /data/dw/sge/submit", "submitDir: sge/submit", 1)
	c, err := loadSGE(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Runtime.SGE.SubmitDir) || !strings.HasSuffix(c.Runtime.SGE.SubmitDir, "/sge/submit") {
		t.Fatalf("submitDir: %q", c.Runtime.SGE.SubmitDir)
	}
}

func TestSGEProblemsAggregate(t *testing.T) {
	body := `
  sge:
    renewFor: 2h
    directDial: true
`
	_, err := loadSGE(t, body)
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"submitDir", "rendezvousDir", "pe is required", "apptainerImage", "renewBefore", "directDial"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("aggregated error must mention %q: %s", want, msg)
		}
	}
}

func TestDefaultSGEWithoutSectionIsRejected(t *testing.T) {
	body := strings.Replace(fmt.Sprintf(baseConfig, ""), "runtime:\n", "runtime:\n  default: sge\n", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("runtime.default: sge without a runtime.sge section must fail at load")
	}
}

func TestUnknownDefaultBackendIsRejected(t *testing.T) {
	body := strings.Replace(fmt.Sprintf(baseConfig, ""), "runtime:\n", "runtime:\n  default: k8s\n", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("an unknown backend must fail at load")
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
