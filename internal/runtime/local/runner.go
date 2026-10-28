package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// RunOptions are per-invocation knobs for Runner.
type RunOptions struct {
	Dir     string
	Env     []string
	Timeout time.Duration
}

// RunResult captures everything callers need to interpret systemctl output
// without a second fork. A non-zero ExitCode is not an error: systemctl uses
// exit status 1/5 for "not loaded", which is a success for Stop.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner is the only way the backend touches systemd, which makes the whole
// unit lifecycle testable with a fake and keeps `systemd-run`/`systemctl`
// optional at runtime.
type Runner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, name string, args []string, o RunOptions) (RunResult, error)
}

type osRunner struct{}

func (osRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (osRunner) Run(ctx context.Context, name string, args []string, o RunOptions) (RunResult, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = o.Dir
	cmd.Env = mergeEnv(os.Environ(), o.Env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			if res.ExitCode < 0 {
				res.ExitCode = 1
			}
			return res, nil
		}
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s: %w", name, ctx.Err())
		}
		return res, fmt.Errorf("%s: %w", name, err)
	}
	return res, nil
}

func mergeEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := map[string]int{}
	for i, kv := range base {
		for j := 0; j < len(kv); j++ {
			if kv[j] == '=' {
				seen[kv[:j]] = i
				break
			}
		}
	}
	out := append([]string{}, base...)
	for _, kv := range extra {
		key := kv
		for j := 0; j < len(kv); j++ {
			if kv[j] == '=' {
				key = kv[:j]
				break
			}
		}
		if i, ok := seen[key]; ok {
			out[i] = kv
			continue
		}
		seen[key] = len(out)
		out = append(out, kv)
	}
	return out
}

// userManagerEnv makes `--user` work when `dw serve` was started from a bare
// context (cron, a system unit, ssh without a login session): both systemd
// and dbus can be reached through the per-user runtime directory.
func userManagerEnv() []string {
	env := []string{}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		env = append(env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		bus := "/run/user/" + strconv.Itoa(os.Getuid()) + "/bus"
		if _, err := os.Stat(bus); err == nil {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
		}
	}
	return env
}
