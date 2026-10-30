package local

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// UnitArgvOptions is everything needed to render the systemd-run argv. It is
// a pure value so golden tests need no systemd and no live backend.
type UnitArgvOptions struct {
	SystemdRun       string
	UnitName         string
	Description      string
	CPU              int
	Memory           string
	TasksMax         int
	StopGrace        time.Duration
	WorkingDirectory string
	LogPath          string
	UnitStdio        string // log | journal
	PATH             string
	Home             string
	DwBin            string
	SpecPath         string
}

var unitNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,180}\.service$`)

// normalizeUnitName accepts either "dw-alice-3f2a1b9c" (old records) or the
// systemd form "dw-alice-3f2a1b9c.service".
func normalizeUnitName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasSuffix(name, ".service") {
		return name
	}
	return name + ".service"
}

func validateUnitName(name string) error {
	if !unitNameRE.MatchString(name) {
		return fmt.Errorf("invalid systemd unit name %q", name)
	}
	return nil
}

// ValidateUnitPath rejects characters that would make the
// StandardOutput=/WorkingDirectory= property or the ExecStopPost= shell
// snippet ambiguous, or that no exec argv can carry. All other bytes are
// passed through unchanged. The manager also calls it for limiter=prlimit and
// limiter=none so a path is rejected before any directory is created.
func ValidateUnitPath(label, p string) error {
	if p == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if strings.ContainsAny(p, "'\"\n\r%\x00") {
		return fmt.Errorf("%s %q contains one of ' \" newline carriage-return %% NUL which cannot be used in a systemd unit property", label, p)
	}
	return nil
}

func verdictProperty(logPath string) string {
	verdict := logPath + ".verdict"
	return `ExecStopPost=-/bin/sh -c "echo $EXIT_STATUS $SERVICE_RESULT > '` + verdict + `'"`
}

// unitArgv renders the exact systemd-run invocation used for unit mode.
//
// `--unit` (not `--scope`) is deliberate: the client exits once the job is
// enqueued, and only a named unit can be stopped/probed by name after a
// control-plane restart. The unit's main process remains `dw instance`, never
// dsh, so dsh's token-bearing stdout stays behind the agent's redactor.
func unitArgv(o UnitArgvOptions) ([]string, error) {
	if o.SystemdRun == "" {
		o.SystemdRun = "systemd-run"
	}
	unit := normalizeUnitName(o.UnitName)
	if err := validateUnitName(unit); err != nil {
		return nil, err
	}
	if o.DwBin == "" {
		return nil, fmt.Errorf("dw binary is required")
	}
	if o.SpecPath == "" {
		return nil, fmt.Errorf("spec path is required")
	}
	if err := ValidateUnitPath("working directory", o.WorkingDirectory); err != nil {
		return nil, err
	}
	if err := ValidateUnitPath("log path", o.LogPath); err != nil {
		return nil, err
	}

	argv := []string{
		o.SystemdRun,
		"--user",
		"--unit=" + unit,
	}
	if o.Description != "" {
		argv = append(argv, "--description="+o.Description)
	}
	argv = append(argv, "--quiet")

	if o.CPU > 0 {
		argv = append(argv, "-p", fmt.Sprintf("CPUQuota=%d%%", o.CPU*100))
	}
	if strings.TrimSpace(o.Memory) != "" {
		mem, err := memoryBytes(o.Memory)
		if err != nil {
			return nil, err
		}
		if mem > 0 {
			argv = append(argv,
				"-p", "MemoryMax="+FormatMemory(mem),
				// Without this MemoryMax is soft: a runaway dsh swaps
				// instead of dying and the instance hangs.
				"-p", "MemorySwapMax=0",
			)
		}
	}
	if o.TasksMax > 0 {
		argv = append(argv, "-p", "TasksMax="+strconv.Itoa(o.TasksMax))
	}
	grace := o.StopGrace
	if grace <= 0 {
		grace = 20 * time.Second
	}
	argv = append(argv,
		// The unit's main process is `dw instance`; dsh is its child.
		// control-group guarantees systemctl stop takes down dsh, node
		// workers and any tool subprocess even if the agent is wedged.
		"-p", "KillMode=control-group",
		// The agent publishes exit_code=143 when it handles SIGTERM, so a
		// controlled `systemctl stop` must not make systemd mark the unit
		// failed. Without this, every normal stop leaves a failed transient
		// unit behind until an explicit reset-failed.
		"-p", "SuccessExitStatus=143",
		"-p", "TimeoutStopSec="+formatTimeSpan(grace),
		// Property form, not --working-directory: the latter only exists in
		// newer systemd and makes every start fail on v239-family hosts.
		"-p", "WorkingDirectory="+o.WorkingDirectory,
	)
	if o.UnitStdio != "journal" {
		// systemd, not us, opens the log: a unit does not inherit our fds.
		// append: preserves today's O_APPEND behaviour and the pre-created
		// 0600 mode.
		argv = append(argv,
			"-p", "StandardOutput=append:"+o.LogPath,
			"-p", "StandardError=append:"+o.LogPath,
		)
	}
	// The verdict outlives the agent: if dsh is OOM-killed, agent and dsh
	// die together and nobody publishes state=failed.
	argv = append(argv, "-p", verdictProperty(o.LogPath))

	if o.PATH != "" {
		argv = append(argv, "--setenv=PATH="+o.PATH)
	}
	if o.Home != "" {
		argv = append(argv,
			"--setenv=HOME="+o.Home,
			"--setenv=DSH_HOME="+o.Home,
		)
	}
	argv = append(argv,
		"--",
		o.DwBin, "instance", "--spec", o.SpecPath,
	)
	return argv, nil
}

// innerCommand is the argv of the agent itself, used both directly (limiter
// none) and as the tail of the prlimit wrapper.
func innerCommand(dwBin, specPath string) []string {
	return []string{dwBin, "instance", "--spec", specPath}
}

func specPath(controlDir string) string {
	return filepath.Join(controlDir, "spec.json")
}
