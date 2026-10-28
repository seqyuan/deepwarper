package local

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseMemory parses the same memory strings dsh/DeepWarper configs use:
// "4Gi", "512Mi", "4G", "1024". It is kept byte-for-byte compatible with the
// pre-systemd implementation so existing configs keep producing the same
// prlimit --as bytes.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty memory")
	}
	mult := int64(1)
	for _, suffix := range []struct {
		s string
		m int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GI", 1 << 30}, {"MI", 1 << 20}, {"KI", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	} {
		if strings.HasSuffix(s, suffix.s) {
			mult = suffix.m
			s = strings.TrimSuffix(s, suffix.s)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int64(n * float64(mult)), nil
}

// FormatMemory renders a byte count as systemd's MemoryMax= value. Bytes are
// used deliberately: there is no G-vs-Gi ambiguity and golden argv tests are
// deterministic across hosts.
func FormatMemory(bytes int64) string { return strconv.FormatInt(bytes, 10) }

// formatTimeSpan renders a duration the way systemd accepts it. Whole
// seconds are emitted without a suffix to match the documented golden argv
// ("TimeoutStopSec=20"); sub-second values keep their unit ("10ms").
func formatTimeSpan(d time.Duration) string {
	if d <= 0 {
		return "1"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10)
	}
	return d.String()
}

// prlimitArgv builds today's prlimit wrapper (same ordering and same flags):
//
//	prlimit [--as=<bytes>] [--cpu=<seconds>] -- <inner...>
//
// When no limit applies the inner argv is returned unchanged, matching the
// old "don't wrap for nothing" behaviour.
func prlimitArgv(prlimitPath string, memBytes int64, cpuSeconds int, inner []string) []string {
	argv := []string{prlimitPath}
	if memBytes > 0 {
		argv = append(argv, fmt.Sprintf("--as=%d", memBytes))
	}
	if cpuSeconds > 0 {
		argv = append(argv, fmt.Sprintf("--cpu=%d", cpuSeconds))
	}
	if len(argv) == 1 {
		return append([]string{}, inner...)
	}
	argv = append(argv, "--")
	return append(argv, inner...)
}

// memoryBytes is a small helper shared by unit and prlimit modes.
func memoryBytes(memory string) (int64, error) {
	if strings.TrimSpace(memory) == "" {
		return 0, nil
	}
	n, err := parseMemory(memory)
	if err != nil {
		return 0, fmt.Errorf("memory %q: %w", memory, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("memory %q: negative", memory)
	}
	return n, nil
}
