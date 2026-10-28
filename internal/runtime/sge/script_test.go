package sge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
)

func TestHeaderSlotsAndHVmem(t *testing.T) {
	cases := []struct {
		name      string
		cpu       int
		memory    string
		account   string
		threads   int
		wantPE    string
		wantLimit string
	}{
		{name: "cores", cpu: 4, memory: "16Gi", account: "cores", wantPE: "smp 4", wantLimit: "h_vmem=4G,h_rt=8:00:00"},
		// 16Gi / 8 slots = 2Gi, raised to the 4Gi floor: the per-slot division is
		// right but RLIMIT_AS needs headroom for the container runtime and V8.
		{name: "threads hits the floor", cpu: 4, memory: "16Gi", account: "threads", threads: 2, wantPE: "smp 8", wantLimit: "h_vmem=4G,h_rt=8:00:00"},
		// 64Gi / 8 slots = 8Gi: above the floor, so no clamping.
		{name: "threads above the floor", cpu: 4, memory: "64Gi", account: "threads", threads: 2, wantPE: "smp 8", wantLimit: "h_vmem=8G,h_rt=8:00:00"},
		// 4Gi / 1 slot = 4Gi, but the floor is 4Gi anyway and rounding up keeps it
		// exact.
		{name: "single slot memory", cpu: 1, memory: "4Gi", account: "cores", wantPE: "smp 1", wantLimit: "h_vmem=4G,h_rt=8:00:00"},
		// 6Gi / 4 = 1.5Gi -> ceil to 2Gi -> floor 4Gi.
		{name: "ceil then floor", cpu: 4, memory: "6Gi", account: "cores", wantPE: "smp 4", wantLimit: "h_vmem=4G,h_rt=8:00:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := testBackend(t)
			b.PEAccounting = c.account
			b.ThreadsPerCore = c.threads
			spec := agent.Spec{UnitName: "dw-alice-7f3a1c2e", CPU: c.cpu, Memory: c.memory, Walltime: "8h", LogPath: "/tmp/job.log"}
			args, err := b.headerArgs(spec)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-pe "+c.wantPE+" ") {
				t.Fatalf("pe: %q", joined)
			}
			if !strings.Contains(joined, "-l "+c.wantLimit) {
				t.Fatalf("limits: %q want %q", joined, c.wantLimit)
			}
		})
	}
}

func TestHeaderSlotsNeverBelowOne(t *testing.T) {
	b := testBackend(t)
	spec := agent.Spec{UnitName: "dw-x", CPU: 0, Memory: "", Walltime: "", LogPath: "/tmp/x.log"}
	args, err := b.headerArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "-pe smp 1") {
		t.Fatalf("argv: %v", args)
	}
}

func TestWalltimeNormalisation(t *testing.T) {
	cases := map[string]string{
		"8h":      "8:00:00",
		"90m":     "1:30:00",
		"1h30m":   "1:30:00",
		"1:30:00": "1:30:00",
		"0:45:00": "0:45:00",
		"45:00":   "0:45:00",
		"90s":     "0:01:30",
	}
	for in, want := range cases {
		got, err := hrtString(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "soon", "-1h", "1:2:3:4", "0:00:00"} {
		if _, err := hrtString(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestHeaderGolden(t *testing.T) {
	b := testBackend(t)
	b.Project = "dwproj"
	b.ExtraQsubArgs = []string{"-w", "e"}
	inst, dist := staticSpec()
	spec := hostSpec(inst, dist, 24105)
	args, err := b.headerArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-N", "dw-alice-7f3a1c2e",
		"-j", "y",
		"-o", spec.LogPath,
		"-e", spec.LogPath,
		"-cwd",
		"-V",
		"-pe", "smp", "4",
		"-l", "h_vmem=4G,h_rt=8:00:00",
		"-q", "batch.q",
		"-P", "dwproj",
		"-w", "e",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv:\n got %q\nwant %q", args, want)
	}
}

func TestScriptGolden(t *testing.T) {
	b := testBackend(t)
	inst, dist := staticSpec()
	spec := hostSpec(inst, dist, 24105)
	script, err := b.renderScript(spec)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "job.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if script != string(want) {
		t.Fatalf("script mismatch:\n--- got ---\n%s\n--- want ---\n%s", script, want)
	}
}

func TestScriptCarriesOnlyExactBinds(t *testing.T) {
	b := testBackend(t)
	b.ExtraBinds = []Bind{{Host: "/mnt/ref", Box: "/ref", ReadOnly: true}}
	inst, dist := staticSpec()
	spec := hostSpec(inst, dist, 24105)
	script, err := b.renderScript(spec)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one --bind per concrete path, never a parent directory.
	want := []string{
		"--bind '" + inst.Home + ":" + b.HomeBox + ":rw'",
		"--bind '" + inst.Workspace + ":" + b.WorkspaceBox + ":rw'",
		"--bind '" + dist + ":" + dist + ":ro'",
		"--bind '" + inst.ControlDir + ":" + inst.ControlDir + ":rw'",
		"--bind '/mnt/ref:/ref:ro'",
	}
	for _, w := range want {
		// The dist bind is rendered without the padding space; normalise it.
		w = strings.ReplaceAll(w, ": ", ":")
		if !strings.Contains(script, w) {
			t.Fatalf("missing bind %q in\n%s", w, script)
		}
	}
	if n := strings.Count(script, "--bind "); n != len(want) {
		t.Fatalf("bind count: %d want %d\n%s", n, len(want), script)
	}
}

func TestScriptHasNoSecretAndNoXTrace(t *testing.T) {
	b := testBackend(t)
	inst, dist := staticSpec()
	script, err := b.renderScript(hostSpec(inst, dist, 24105))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "set -x", "cat \"$RD/spec.json\"", "echo $("} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script must not contain %q:\n%s", forbidden, script)
		}
	}
	if n := strings.Count(script, "instance --spec"); n != 1 {
		t.Fatalf("dw instance --spec must appear exactly once, got %d", n)
	}
}

func TestScriptDoesNotExecContainer(t *testing.T) {
	b := testBackend(t)
	inst, dist := staticSpec()
	script, err := b.renderScript(hostSpec(inst, dist, 24105))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		// `exec` would replace the script's shell and skip the exit-code
		// bookkeeping below it.
		if strings.HasPrefix(trimmed, "exec ") {
			t.Fatalf("script must not exec the container: %q", line)
		}
	}
	execAt := strings.Index(script, "\"$DW_BOX\" exec")
	if execAt < 0 {
		t.Fatal("container run line missing")
	}
	tail := script[execAt:]
	if !strings.Contains(tail, "_pub state exited") || !strings.Contains(tail, "_pub exit_code \"$_code\"") {
		t.Fatalf("exit code must be published after the container run:\n%s", tail)
	}
}

func TestScriptResolvesContainerOnComputeNode(t *testing.T) {
	b := testBackend(t)
	inst, dist := staticSpec()
	script, err := b.renderScript(hostSpec(inst, dist, 24105))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "command -v apptainer || command -v singularity") {
		t.Fatalf("apptainer must be resolved on the compute node:\n%s", script)
	}
}

func TestScriptPublishesNodeAndJobID(t *testing.T) {
	b := testBackend(t)
	inst, dist := staticSpec()
	script, err := b.renderScript(hostSpec(inst, dist, 24105))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `_pub node "$(hostname)"`) {
		t.Fatal("the job must publish the compute node hostname from the host side")
	}
	if !strings.Contains(script, `_pub jobid "${JOB_ID:-}"`) {
		t.Fatal("the job must publish its JOB_ID")
	}
	if !strings.Contains(script, "trap '_pub state failed; _pub exit_code 143' TERM INT") {
		t.Fatal("a killed job must publish a terminal state")
	}
}

// TestBoxSpecPortZeroLoginPort guards the trap where the job is told to bind the
// login node's tunnel port: it would collide with the ssh end on the login node.
func TestBoxSpecPortZeroLoginPort(t *testing.T) {
	b := testBackend(t)
	inst, dist := staticSpec()
	host := hostSpec(inst, dist, 24105)
	box := b.boxSpec(host)
	if box.Port != 0 {
		t.Fatalf("box spec must ask for a free port, got %d", box.Port)
	}
	if box.LoginPort != 24105 {
		t.Fatalf("login port: %d", box.LoginPort)
	}
	if box.Home != b.HomeBox || box.Workspace != b.WorkspaceBox || box.DshBin != b.DSHBox {
		t.Fatalf("box paths: %+v", box)
	}
	if box.ControlDir != host.ControlDir || box.LogPath != filepath.Join(host.ControlDir, "job.log") {
		t.Fatalf("control paths must be identity-mapped: %+v", box)
	}
	if !strings.Contains(box.PatchContent, b.WorkspaceBox) {
		t.Fatalf("runtime patch must use the box workspace: %q", box.PatchContent)
	}
}

func TestParseQsubOutput(t *testing.T) {
	ok := map[string]string{
		"12345": "12345",
		"Your job 12345 (\"dw-alice-7f3a1c2e\") has been submitted": "12345",
		"12345.1-1": "12345.1-1",
		"Your job-array 12345.1-10:1 has been submitted": "12345.1-10:1",
		"warning: something\n12345\n":                    "12345",
	}
	for in, want := range ok {
		got, err := parseQsubOutput(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
	// The trap: "syntax error near line 3" must not become job 3.
	for _, bad := range []string{"", "syntax error near line 3", "qsub: bad -pe value 3"} {
		if id, err := parseQsubOutput(bad); err == nil {
			t.Fatalf("%q must be rejected, got job id %q", bad, id)
		}
	}
}

func TestFormatHVmem(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{4 << 30, "4G"},
		{1536 << 20, "1536M"},
		{(3 << 30) / 2, "1536M"},
		{1024, "1M"}, // rounded up to whole MiB so two hosts agree
		{0, "0M"},
	}
	for _, c := range cases {
		if got := formatHVmem(c.in); got != c.want {
			t.Fatalf("formatHVmem(%d) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestParseWalltimeAcceptsSeconds(t *testing.T) {
	if d, err := parseWalltime("0:00:30"); err != nil || d != 30*time.Second {
		t.Fatalf("%v/%v", d, err)
	}
}
