package local

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUnitArgvGolden(t *testing.T) {
	argv, err := unitArgv(UnitArgvOptions{
		SystemdRun:       "systemd-run",
		UnitName:         "dw-alice-3f2a1b9c.service",
		Description:      "deepwarper alice-default",
		CPU:              2,
		Memory:           "4Gi",
		TasksMax:         512,
		StopGrace:        20 * time.Second,
		WorkingDirectory: "/tmp/dw-prod/workspaces/alice",
		LogPath:          "/tmp/dw-prod/logs/alice-default.log",
		UnitStdio:        "log",
		PATH:             "/usr/local/bin:/usr/bin:/bin",
		Home:             "/tmp/dw-prod/homes/alice",
		DwBin:            "/path/to/dw",
		SpecPath:         "/tmp/dw-prod/control/alice-default/spec.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemd-run",
		"--user",
		"--unit=dw-alice-3f2a1b9c.service",
		"--description=deepwarper alice-default",
		"--quiet",
		"-p", "CPUQuota=200%",
		"-p", "MemoryMax=4294967296",
		"-p", "MemorySwapMax=0",
		"-p", "TasksMax=512",
		"-p", "KillMode=control-group",
		"-p", "SuccessExitStatus=143",
		"-p", "TimeoutStopSec=20",
		"-p", "WorkingDirectory=/tmp/dw-prod/workspaces/alice",
		"-p", "StandardOutput=append:/tmp/dw-prod/logs/alice-default.log",
		"-p", "StandardError=append:/tmp/dw-prod/logs/alice-default.log",
		"-p", `ExecStopPost=-/bin/sh -c "echo $EXIT_STATUS $SERVICE_RESULT > '/tmp/dw-prod/logs/alice-default.log.verdict'"`,
		"--setenv=PATH=/usr/local/bin:/usr/bin:/bin",
		"--setenv=HOME=/tmp/dw-prod/homes/alice",
		"--setenv=DSH_HOME=/tmp/dw-prod/homes/alice",
		"--",
		"/path/to/dw", "instance", "--spec", "/tmp/dw-prod/control/alice-default/spec.json",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", argv, want)
	}
}

func TestUnitArgvOmitsAbsentLimits(t *testing.T) {
	argv, err := unitArgv(UnitArgvOptions{
		UnitName:         "dw-bob-00000000.service",
		StopGrace:        20 * time.Second,
		WorkingDirectory: "/tmp/ws",
		LogPath:          "/tmp/log",
		DwBin:            "/usr/bin/dw",
		SpecPath:         "/tmp/spec.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, forbidden := range []string{"CPUQuota", "MemoryMax", "MemorySwapMax", "TasksMax"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("%s should be omitted for absent limits: %v", forbidden, argv)
		}
	}
}

func TestUnitArgvNeverSetsNProc(t *testing.T) {
	argv, err := unitArgv(UnitArgvOptions{
		UnitName:         "dw-bob-00000000.service",
		CPU:              2,
		Memory:           "4Gi",
		StopGrace:        20 * time.Second,
		WorkingDirectory: "/tmp/ws",
		LogPath:          "/tmp/log",
		DwBin:            "/usr/bin/dw",
		SpecPath:         "/tmp/spec.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.ToLower(strings.Join(argv, " "))
	for _, forbidden := range []string{"nproc", "ulimit"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unit argv must not use %s: %v", forbidden, argv)
		}
	}
}

func TestPrlimitArgvGolden(t *testing.T) {
	inner := innerCommand("/x/dw", "/x/spec.json")
	got := prlimitArgv("prlimit", 4294967296, 0, inner)
	want := []string{"prlimit", "--as=4294967296", "--", "/x/dw", "instance", "--spec", "/x/spec.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	got = prlimitArgv("prlimit", 0, 7200, inner)
	want = []string{"prlimit", "--cpu=7200", "--", "/x/dw", "instance", "--spec", "/x/spec.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	got = prlimitArgv("prlimit", 0, 0, inner)
	if !reflect.DeepEqual(got, inner) {
		t.Fatalf("no limits should not wrap: %#v", got)
	}
}

func TestFormatMemory(t *testing.T) {
	cases := map[string]int64{
		"4Gi":   4294967296,
		"512Mi": 536870912,
		"4G":    4294967296,
		"1024":  1024,
	}
	for in, want := range cases {
		got, err := memoryBytes(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("%s: got %d want %d", in, got, want)
		}
		if FormatMemory(got) == "" {
			t.Fatalf("%s: empty format", in)
		}
	}
	if _, err := memoryBytes("nope"); err == nil {
		t.Fatal("expected error for bad memory")
	}
}

func TestUnitNameValidation(t *testing.T) {
	valid := []string{"dw-alice-3f2a1b9c.service", "dw-bob-00000000.service"}
	for _, name := range valid {
		if err := validateUnitName(name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	invalid := []string{"", "/dw-alice.service", "dw alice.service", ".dw.service", "dw-alice", "dw-alice.service\n"}
	for _, name := range invalid {
		if err := validateUnitName(name); err == nil {
			t.Fatalf("%q should be invalid", name)
		}
	}
	if got := normalizeUnitName("dw-alice-3f2a1b9c"); got != "dw-alice-3f2a1b9c.service" {
		t.Fatalf("normalize: %q", got)
	}
}

func TestUnitArgvRejectsUnsafePaths(t *testing.T) {
	base := UnitArgvOptions{
		UnitName:         "dw-bob-00000000.service",
		WorkingDirectory: "/tmp/ws",
		LogPath:          "/tmp/log",
		DwBin:            "/usr/bin/dw",
		SpecPath:         "/tmp/spec.json",
	}
	for _, bad := range []string{`/tmp/a'b`, `/tmp/a"b`, "/tmp/a\nb", "/tmp/a%b"} {
		o := base
		o.LogPath = bad
		if _, err := unitArgv(o); err == nil {
			t.Fatalf("log path %q should be rejected", bad)
		}
	}
}
