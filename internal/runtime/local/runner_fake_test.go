package local

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

type fakeCall struct {
	name string
	args []string
}

type fakeRunner struct {
	mu      sync.Mutex
	paths   map[string]string
	handler func(name string, args []string) (RunResult, error)
	calls   []fakeCall
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.paths == nil {
		return name, nil
	}
	if p, ok := f.paths[name]; ok {
		return p, nil
	}
	return "", exec.ErrNotFound
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, o RunOptions) (RunResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string{}, args...)})
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(name, args)
	}
	return RunResult{}, nil
}

func (f *fakeRunner) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall{}, f.calls...)
}

func hasCall(f *fakeRunner, name string, substr string) bool {
	for _, c := range f.recorded() {
		if c.name != name {
			continue
		}
		if substr == "" || strings.Contains(strings.Join(c.args, " "), substr) {
			return true
		}
	}
	return false
}

func systemctlArgs(args []string) (verb string, rest []string) {
	for i, a := range args {
		if a == "--user" {
			if i+1 < len(args) {
				return args[i+1], args[i+2:]
			}
		}
	}
	if len(args) > 0 {
		return args[0], args[1:]
	}
	return "", nil
}

func showOutput(active string, mainPID int, result string, exit int) RunResult {
	return RunResult{Stdout: "LoadState=loaded\n" +
		"ActiveState=" + active + "\n" +
		"SubState=running\n" +
		"MainPID=" + itoa(mainPID) + "\n" +
		"Result=" + result + "\n" +
		"ExecMainStatus=" + itoa(exit) + "\n"}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func lifecycleHandler(active string, mainPID int) func(string, []string) (RunResult, error) {
	return func(name string, args []string) (RunResult, error) {
		switch name {
		case "systemd-run":
			return RunResult{}, nil
		case "systemctl":
			verb, _ := systemctlArgs(args)
			switch verb {
			case "show":
				return showOutput(active, mainPID, "success", 0), nil
			default:
				return RunResult{}, nil
			}
		}
		return RunResult{}, nil
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return "."
	}
	return path[:i]
}
