package local

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStartFailsWhenBwrapMissing(t *testing.T) {
	spec := fallbackSpec(t)
	spec.BwrapArgv = []string{"bwrap", "--bind", spec.Workspace, "/workspace", "--"}
	f := &fakeRunner{paths: map[string]string{"prlimit": "/usr/bin/prlimit"}}
	b := New(Options{DwBin: "/bin/true", Limiter: "none", Run: f})
	_, err := b.Start(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("error: %v", err)
	}
}

func TestStartKeepsDwCommandWithoutBwrap(t *testing.T) {
	spec := fallbackSpec(t)
	b := New(Options{DwBin: fakeDw(t), Limiter: "none", ReadyWait: 5 * time.Second})
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	cmd := h.Command()
	if len(cmd) == 0 || strings.Contains(cmd[0], "bwrap") {
		t.Fatalf("command: %#v", cmd)
	}
}
