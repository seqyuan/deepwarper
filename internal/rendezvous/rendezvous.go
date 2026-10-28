package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Ready struct {
	Version     int       `json:"version"`
	Incarnation string    `json:"incarnation"`
	State       string    `json:"state"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	PID         int       `json:"pid"`
	Node        string    `json:"node,omitempty"`
	Profile     string    `json:"profile"`
	DshVersion  string    `json:"dsh_version,omitempty"`
	PublishedAt time.Time `json:"published_at"`
}

func Clear(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func Write(dir, name string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".tmp-"+name+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func WriteString(dir, name, value string, mode os.FileMode) error {
	return Write(dir, name, []byte(value+"\n"), mode)
}

func Read(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func WriteReady(dir string, r Ready) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return Write(dir, "ready.json", data, 0o644)
}

func ReadReady(dir string) (*Ready, error) {
	data, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	if err != nil {
		return nil, err
	}
	var r Ready
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// LivenessProbe is consulted while waiting for ready.json. Returning
// gone=true aborts the wait with the status string as the explanation. This
// lets the local backend report a systemd OOM-kill long before the ready
// timeout expires.
type LivenessProbe func() (status string, gone bool)

func WaitReady(ctx context.Context, dir, incarnation string, timeout time.Duration) (*Ready, error) {
	return WaitReadyProbe(ctx, dir, incarnation, timeout, nil)
}

func WaitReadyProbe(ctx context.Context, dir, incarnation string, timeout time.Duration, probe LivenessProbe) (*Ready, error) {
	deadline := time.Now().Add(timeout)
	for {
		r, err := ReadReady(dir)
		if err == nil && r.Incarnation == incarnation && r.State == "ready" && r.Port > 0 {
			return r, nil
		}
		if err == nil && r.Incarnation == incarnation && (r.State == "failed" || r.State == "exited") {
			return nil, fmt.Errorf("instance %s: %s", r.Incarnation, r.State)
		}
		state, _ := Read(dir, "state")
		if state == "failed" || state == "exited" {
			code, _ := Read(dir, "exit_code")
			return nil, fmt.Errorf("instance exited before ready (state=%s exit=%s)", state, code)
		}
		if probe != nil {
			if status, gone := probe(); gone {
				return nil, fmt.Errorf("instance %s: %s", incarnation, status)
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for ready.json")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
