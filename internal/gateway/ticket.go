package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ticketGrace is how long an unparsable or expired ticket may linger
	// before the janitor removes it; it absorbs in-flight launches.
	ticketGrace = time.Minute
	// ticketUsedRetentionFloor is the minimum time a consumed ticket marker is
	// kept: the 24h maximum ticket TTL allowed by config validation plus a 1h
	// margin. The marker must outlive the longest possible ticket because the
	// .json file is removed only after the marker is claimed. If that removal
	// ever fails, pruning the marker early would let the still-parseable ticket
	// be replayed.
	ticketUsedRetentionFloor = 25 * time.Hour
	// atomicTempPattern is the prefix of atomic issue temp files.
	atomicTempPattern = ".ticket-tmp-"
)

// writeFileAtomic writes data to path through a temp file in dir and renames it
// into place with the requested mode (0600). Readers never see a partial file.
func writeFileAtomic(dir, path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, atomicTempPattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cleanupTickets removes expired tickets, stale consumption markers and
// leftover temp files. Valid unconsumed tickets are never touched.
// ticketUsedRetention derives the consumed-marker retention from the configured
// ticket TTL so a raised TTL can never widen the replay window past the marker
// lifetime. The floor covers the 24h validation ceiling and a nil config.
func (g *Gateway) ticketUsedRetention() time.Duration {
	var ttl time.Duration
	if g != nil && g.cfg != nil {
		ttl = g.cfg.TicketTTL()
	}
	retention := ttl + 2*ticketGrace
	if retention < ticketUsedRetentionFloor {
		retention = ticketUsedRetentionFloor
	}
	return retention
}

func (g *Gateway) cleanupTickets(now time.Time) error {
	retention := g.ticketUsedRetention()
	entries, err := os.ReadDir(g.ticketDir)
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
		name := e.Name()
		path := filepath.Join(g.ticketDir, name)
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		switch {
		case strings.HasPrefix(name, atomicTempPattern):
			if now.Sub(info.ModTime()) > ticketGrace {
				_ = os.Remove(path)
			}
		case strings.HasSuffix(name, ".used"):
			if now.Sub(info.ModTime()) > retention {
				_ = os.Remove(path)
			}
		case strings.HasSuffix(name, ".json"):
			if g.ticketExpired(path, info, now) {
				_ = os.Remove(path)
			}
		}
	}
	return nil
}

// ticketExpired uses the recorded expiry when the file parses and falls back
// to the mtime for corrupt files.
func (g *Gateway) ticketExpired(path string, info os.FileInfo, now time.Time) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var rec ticketRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Exp.IsZero() {
		return now.Sub(info.ModTime()) > ticketGrace
	}
	return now.After(rec.Exp.Add(ticketGrace))
}

// RunTicketJanitor periodically prunes the ticket directory until ctx is
// canceled. main starts it with the serve context.
func (g *Gateway) RunTicketJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := g.cleanupTickets(now); err != nil {
				g.logf("ticket janitor: %v", err)
			}
		}
	}
}
