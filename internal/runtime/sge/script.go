package sge

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// headerArgs renders the qsub resource request. This is where an instance's
// declaration becomes the scheduler's language; nothing downstream re-derives
// it.
func (b *Backend) headerArgs(spec agent.Spec) ([]string, error) {
	slots := b.slotsFor(spec.CPU)
	args := []string{
		"-N", spec.UnitName,
		"-j", "y", // merge stderr into one log stream
		"-o", spec.LogPath,
		"-e", spec.LogPath,
		"-cwd",
		"-V", // forward the submission environment (the container clears it)
	}
	args = append(args, "-pe", b.PE, strconv.Itoa(slots))
	limits, err := b.resourceLimits(spec.CPU, spec.Memory, spec.Walltime, spec.GPU)
	if err != nil {
		return nil, err
	}
	args = append(args, limits...)
	if q := strings.TrimSpace(b.Queue); q != "" {
		args = append(args, "-q", q)
	}
	if p := strings.TrimSpace(b.Project); p != "" {
		args = append(args, "-P", p)
	}
	args = append(args, b.ExtraQsubArgs...)
	return args, nil
}

// resourceLimits renders the `-l` resource list.
//
// One renderer is shared by qsub and qalter because `qalter -l` *replaces* a
// job's resource list: altering only h_rt drops h_vmem and the scheduler refuses
// the change for a running job ("former resource request on consumable h_vmem
// ... missing in new resource request"). Keeping the renderer single is what
// keeps the two in step.
func (b *Backend) resourceLimits(cpu int, memory, walltime string, gpu int) ([]string, error) {
	var items []string
	if strings.TrimSpace(memory) != "" {
		total, err := config.ParseMemory(memory)
		if err != nil {
			return nil, fmt.Errorf("instance memory: %w", err)
		}
		slots := b.slotsFor(cpu)
		// h_vmem is a per-slot limit: a request for N slots and M total needs
		// M/N per slot. Round up (truncation causes OOM) and then apply the
		// floor, because h_vmem is enforced as RLIMIT_AS and a container runtime
		// plus Node/V8 reserve a lot of virtual address space.
		perSlot := (total + uint64(slots) - 1) / uint64(slots)
		if perSlot < b.MinHVmemPerSlot {
			perSlot = b.MinHVmemPerSlot
		}
		items = append(items, "h_vmem="+formatHVmem(perSlot))
	}
	if strings.TrimSpace(walltime) != "" {
		hrt, err := hrtString(walltime)
		if err != nil {
			return nil, err
		}
		items = append(items, "h_rt="+hrt)
	}
	if gpu > 0 && b.GPUFlag != "" && !strings.EqualFold(b.GPUFlag, "none") {
		items = append(items, fmt.Sprintf("%s=%d", b.GPUFlag, gpu))
	}
	if len(items) == 0 {
		return nil, nil
	}
	return []string{"-l", strings.Join(items, ",")}, nil
}

// formatHVmem renders a byte count as h_vmem. Powers of 1024 are used because
// that is what SGE means by G/M; anything else is rounded up to whole MiB so two
// hosts produce the same golden string.
func formatHVmem(bytes uint64) string {
	const (
		gi = 1 << 30
		mi = 1 << 20
	)
	switch {
	case bytes == 0:
		return "0M"
	case bytes%gi == 0:
		return strconv.FormatUint(bytes/gi, 10) + "G"
	case bytes%mi == 0:
		return strconv.FormatUint(bytes/mi, 10) + "M"
	default:
		return strconv.FormatUint((bytes+mi-1)/mi, 10) + "M"
	}
}

// parseWalltime accepts a Go duration ("8h", "90m", "1h30m") or SGE's own
// "H:MM:SS" / "MM:SS" spelling.
func parseWalltime(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty walltime")
	}
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		if len(parts) > 3 {
			return 0, fmt.Errorf("invalid walltime %q (want H:MM:SS or a Go duration)", raw)
		}
		// Parse from the right: seconds, then minutes, then hours.
		total := time.Duration(0)
		mult := time.Second
		for i := len(parts) - 1; i >= 0; i-- {
			n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid walltime %q (want H:MM:SS or a Go duration)", raw)
			}
			total += time.Duration(n) * mult
			mult *= 60
		}
		if total <= 0 {
			return 0, fmt.Errorf("invalid walltime %q: must be positive", raw)
		}
		return total, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid walltime %q (want H:MM:SS or a Go duration like 8h)", raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid walltime %q: must be positive", raw)
	}
	return d, nil
}

// formatHRT renders a duration the way SGE's h_rt wants it.
func formatHRT(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d:%02d", secs/3600, (secs%3600)/60, secs%60)
}

// hrtString normalises a declared walltime into SGE's H:MM:SS.
func hrtString(walltime string) (string, error) {
	d, err := parseWalltime(walltime)
	if err != nil {
		return "", err
	}
	return formatHRT(d), nil
}

// binds is the exact set of container mounts: one --bind per concrete path,
// never a parent directory.
//
// The control directory is bound at its own path so spec.json, ready.json,
// state, incarnation and exit_code need no translation: `dw instance` writes
// where the login node reads.
func (b *Backend) binds(host agent.Spec) ([]Bind, error) {
	var out []Bind
	add := func(hostPath, boxPath string, ro bool) error {
		if strings.TrimSpace(hostPath) == "" || strings.TrimSpace(boxPath) == "" {
			return fmt.Errorf("bind with an empty path (host=%q box=%q)", hostPath, boxPath)
		}
		if !filepath.IsAbs(hostPath) || !filepath.IsAbs(boxPath) {
			return fmt.Errorf("bind paths must be absolute (host=%q box=%q)", hostPath, boxPath)
		}
		if hostPath == "/" || boxPath == "/" {
			return fmt.Errorf("refusing to bind / (host=%q box=%q)", hostPath, boxPath)
		}
		out = append(out, Bind{Host: hostPath, Box: boxPath, ReadOnly: ro})
		return nil
	}
	if err := add(host.Home, b.HomeBox, false); err != nil {
		return nil, err
	}
	if err := add(host.Workspace, b.WorkspaceBox, false); err != nil {
		return nil, err
	}
	if host.DistPath != "" {
		// Bind the distribution at the identical absolute path. The per-user
		// profile's node_modules is a symlink created on the login node; if the
		// container exposed the dist under a different path, that symlink would
		// dangle inside the job. Identity binding keeps local and SGE profiles
		// interchangeable without rewriting user homes.
		if err := add(host.DistPath, host.DistPath, true); err != nil {
			return nil, err
		}
	}
	if err := add(host.ControlDir, host.ControlDir, false); err != nil {
		return nil, err
	}
	for _, extra := range b.ExtraBinds {
		if err := add(extra.Host, extra.Box, extra.ReadOnly); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// boxSpec is the spec.json that `dw instance` reads *inside* the container.
//
// Port is 0 so the agent picks a free loopback port on the compute node; the
// login node's pool port travels as LoginPort, which the job never binds (that
// would collide with the tunnel end on the login node).
func (b *Backend) boxSpec(host agent.Spec) agent.Spec {
	box := host
	box.LoginPort = host.Port
	box.Port = 0
	box.Home = b.HomeBox
	box.Workspace = b.WorkspaceBox
	box.DshBin = b.DSHBox
	box.LogPath = filepath.Join(host.ControlDir, "job.log")
	box.PatchContent = runtime.RuntimePatch(b.WorkspaceBox)
	return box
}

// renderScript builds the job script.
//
// Non-negotiable properties, each asserted by a test:
//
//  1. No secret anywhere in the script. The dsh launch secret is produced by
//     `dw instance` at runtime into <controlDir>/token; the script never echoes
//     spec.json and never enables `set -x`.
//  2. The container is not `exec`ed: `exec` would replace the script's shell and
//     silently skip publishing state=exited and exit_code.
//  3. apptainer is resolved on the compute node, never on the login node.
//  4. Exactly one --bind per concrete path, never a parent directory.
//  5. `dw instance --spec` is the only thing executed, so dsh's own CLI cannot
//     force an edit here.
//  6. The job-side port is chosen by `dw instance` on the compute node and
//     published in ready.json; there is no port placeholder to restore.
func (b *Backend) renderScript(host agent.Spec) (string, error) {
	binds, err := b.binds(host)
	if err != nil {
		return "", err
	}
	specPath := filepath.Join(host.ControlDir, "spec.json")

	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("#$ -S /bin/bash\n")
	sb.WriteString("# generated by dw; regenerated on every start, edits are overwritten.\n")
	sb.WriteString("set -uo pipefail\n\n")
	fmt.Fprintf(&sb, "RD=%s\n", shellQuote(host.ControlDir))
	fmt.Fprintf(&sb, "INC=%s\n", shellQuote(host.Incarnation))
	sb.WriteString("mkdir -p \"$RD\"\n")
	sb.WriteString("_pub() { local f=\"$RD/$1.tmp\"; printf '%s\\n' \"$2\" >\"$f\" && mv \"$f\" \"$RD/$1\"; }\n")
	sb.WriteString("trap '_pub state failed; _pub exit_code 143' TERM INT\n\n")
	sb.WriteString("# The shared filesystem is the control channel: the login node reads these.\n")
	sb.WriteString("_pub incarnation \"$INC\"\n")
	sb.WriteString("_pub state starting\n")
	sb.WriteString("_pub node \"$(hostname)\"\n")
	sb.WriteString("_pub jobid \"${JOB_ID:-}\"\n")
	sb.WriteString("_pub pid \"$$\"\n\n")
	sb.WriteString("# The container runtime is installed on compute nodes, not on the login node.\n")
	sb.WriteString("DW_BOX=\"$(command -v apptainer || command -v singularity || true)\"\n")
	sb.WriteString("if [ -z \"$DW_BOX\" ]; then\n")
	sb.WriteString("  _pub state failed; _pub exit_code 1\n")
	sb.WriteString("  echo \"dw: neither apptainer nor singularity on $(hostname)\" >&2\n")
	sb.WriteString("  exit 1\n")
	sb.WriteString("fi\n")
	fmt.Fprintf(&sb, "SIF=%s\n", shellQuote(b.ApptainerImage))
	sb.WriteString("if [ ! -r \"$SIF\" ]; then\n")
	sb.WriteString("  _pub state failed; _pub exit_code 1\n")
	sb.WriteString("  echo \"dw: image not readable on $(hostname): $SIF\" >&2\n")
	sb.WriteString("  exit 1\n")
	sb.WriteString("fi\n\n")

	fmt.Fprintf(&sb, "\"$DW_BOX\" exec %s \\\n", shellJoin(b.ContainerArgs))
	fmt.Fprintf(&sb, "  --home %s --pwd %s \\\n", shellQuote(b.HomeBox), shellQuote(b.WorkspaceBox))
	for _, bind := range binds {
		fmt.Fprintf(&sb, "  --bind %s \\\n", shellQuote(bind.Flag()))
	}
	fmt.Fprintf(&sb, "  \"$SIF\" \\\n")
	fmt.Fprintf(&sb, "  %s instance --spec %s\n\n", shellQuote(b.DWBox), shellQuote(specPath))
	sb.WriteString("_code=$?\n")
	sb.WriteString("_pub state exited\n")
	sb.WriteString("_pub exit_code \"$_code\"\n")
	sb.WriteString("exit \"$_code\"\n")
	return sb.String(), nil
}

// shellQuote quotes one argument so a path containing a space or a quote cannot
// change the command it belongs to.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellArg quotes one argument only when it needs quoting, so a generated script
// stays readable for the common case (flags, absolute paths) while a value with a
// space or a quote can never change the command it belongs to.
func shellArg(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '@', r == '%', r == '+', r == '=', r == ':', r == ',', r == '.', r == '/', r == '-':
		default:
			return shellQuote(s)
		}
	}
	return s
}

func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellArg(a)
	}
	return strings.Join(parts, " ")
}

// parseQsubOutput extracts the job id from qsub's stdout.
//
// Sites differ in what they print:
//
//	12345
//	Your job 12345 ("dw-alice-3f2a1b9c") has been submitted
//	12345.1-1                                  (cell/task suffixed)
//	Your job-array 12345.1-10:1 has been submitted
//
// A bare "first integer on the line" rule is wrong: a warning such as
// "syntax error near line 3" would yield job 3 and the backend would then track a
// job that does not exist. The number is therefore accepted only when the line is
// exactly a job id, or when it directly follows the word "job".
func parseQsubOutput(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if id, ok := jobIDToken(line); ok {
			return id, nil
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			// Job-array submissions say "job-array", so any "job*" marker counts.
			word := strings.ToLower(strings.Trim(f, "(),:\"'"))
			if !strings.HasPrefix(word, "job") || i+1 >= len(fields) {
				continue
			}
			if id, ok := jobIDToken(fields[i+1]); ok {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("no job id in qsub output")
}

// jobIDToken accepts a bare integer, optionally with a .task-range suffix.
func jobIDToken(tok string) (string, bool) {
	tok = strings.Trim(tok, "().,\"'")
	if tok == "" {
		return "", false
	}
	head, _, _ := strings.Cut(tok, ".")
	if _, err := strconv.Atoi(head); err != nil {
		return "", false
	}
	return tok, true
}
