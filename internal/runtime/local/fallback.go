package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
)

// startChild is the pre-systemd code path, kept deliberately close to its
// original shape: prlimit wrapper (optional), O_APPEND log inherited by the
// child, Setpgid so Stop can signal the whole group.
func (b *Backend) startChild(ctx context.Context, spec agent.Spec, limiter string) (*Handle, error) {
	inner := innerCommand(b.DwBin, specPath(spec.ControlDir))
	argv := inner
	if limiter == "prlimit" {
		if _, err := b.runner().LookPath("prlimit"); err == nil {
			mem, err := memoryBytes(spec.Memory)
			if err != nil {
				return nil, err
			}
			// prlimitPath is intentionally the bare name: existing records
			// and logs keep the historical argv spelling.
			argv = prlimitArgv("prlimit", mem, b.PrlimitCPUSeconds, inner)
		} else {
			limiter = "none"
		}
	}

	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Env = append(os.Environ(),
		"DSH_HOME="+spec.Home,
		"HOME="+spec.Home,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	h := &Handle{
		backend:     b,
		mode:        modeProcess,
		cmd:         cmd,
		controlDir:  spec.ControlDir,
		incarnation: spec.Incarnation,
		logPath:     spec.LogPath,
		command:     argv,
		limiter:     limiter,
		pid:         cmd.Process.Pid,
		pidStart:    pidStartTime(cmd.Process.Pid),
		done:        make(chan struct{}),
	}
	go func() {
		h.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(h.done)
	}()
	return h, nil
}

// pidStartTime reads field 22 (starttime) from /proc/<pid>/stat. It is used
// to reject a reused pid, which otherwise lets a stale record signal an
// unrelated process.
func pidStartTime(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	// comm (field 2) is parenthesised and may contain spaces or ')'. The
	// last ')' is the end of comm.
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is field 3 (state); starttime is field 22 => index 19.
	if len(fields) <= 19 {
		return 0
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func processAlive(pid int, start uint64) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if start > 0 {
		actual := pidStartTime(pid)
		if actual == 0 || actual != start {
			return false
		}
	}
	return true
}

func readCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}

// stopRecordedProcess stops a process recorded on disk after the in-memory
// handle is gone. A mismatched start time means "that pid is someone else":
// the function reports success without signalling.
func stopRecordedProcess(ctx context.Context, pid int, start uint64, specPath string) error {
	if pid <= 0 {
		return nil
	}
	if !processAlive(pid, 0) {
		return nil
	}
	if start > 0 {
		if pidStartTime(pid) != start {
			return nil
		}
	} else if specPath != "" {
		// Legacy record without pid_start: only signal when the command line
		// still points at this instance's spec file.
		line := readCmdline(pid)
		if line == "" || !strings.Contains(line, specPath) {
			return nil
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if syscall.Kill(pid, 0) != nil {
		return nil
	}
	if waitProcessGone(ctx, pid, 5*time.Second) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if waitProcessGone(ctx, pid, 5*time.Second) {
		return nil
	}
	return errors.New("process did not exit")
}

func waitProcessGone(ctx context.Context, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return false
		}
	}
}

// stopChild is the original Stop: SIGTERM the group, wait 5s, SIGKILL, wait
// 5s. The timings are unchanged on purpose.
func (h *Handle) stopChild(ctx context.Context) error {
	if h.cmd == nil || h.cmd.Process == nil {
		return nil
	}
	select {
	case <-h.done:
		return nil
	default:
	}
	_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-h.done:
		return nil
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-h.done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("dsh did not exit")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ProbeRecord is kept for callers that only have a control dir; the manager
// now uses Backend.Probe, which can tell genuine unit death from a missing
// ready.json.
func ProbeRecord(controlDir, stateFile string) (string, error) {
	state, err := rendezvous.Read(controlDir, "state")
	if err != nil {
		return "", err
	}
	if state == "ready" {
		if _, err := rendezvous.ReadReady(controlDir); err != nil {
			return "starting", nil
		}
	}
	return state, nil
}
