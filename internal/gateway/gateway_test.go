package gateway

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/config"
)

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	dir := t.TempDir()
	g := &Gateway{
		key:       []byte("0123456789abcdef0123456789abcdef"),
		ticketDir: dir,
		cfg:       &config.Config{Platform: config.PlatformConfig{SessionTTL: "1h"}},
	}
	return g
}

func TestSessionRoundTripAndAuthorityBinding(t *testing.T) {
	g := testGateway(t)
	value, err := g.signSession("alice", "alice.localhost:7658")
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := g.verifySession(value, "alice.localhost:7658"); !ok || user != "alice" {
		t.Fatalf("verify failed: %q %v", user, ok)
	}
	if _, ok := g.verifySession(value, "bob.localhost:7658"); ok {
		t.Fatal("authority binding did not reject another host")
	}
	if _, ok := g.verifySession(value+"x", "alice.localhost:7658"); ok {
		t.Fatal("tampered cookie accepted")
	}
}

func TestTicketIsSingleUse(t *testing.T) {
	g := testGateway(t)
	rec := ticketRecord{User: "alice", InstanceID: "alice-default", Authority: "alice.localhost:7658", Exp: time.Now().Add(time.Minute)}
	data, _ := json.Marshal(rec)
	raw := "ticket-1"
	if err := os.WriteFile(g.ticketPath(raw), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.consumeTicket(raw, "alice.localhost:7658"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.consumeTicket(raw, "alice.localhost:7658"); err == nil {
		t.Fatal("ticket accepted twice")
	}
}

func TestExpiredTicketRejected(t *testing.T) {
	g := testGateway(t)
	rec := ticketRecord{User: "alice", InstanceID: "alice-default", Authority: "alice.localhost:7658", Exp: time.Now().Add(-time.Minute)}
	data, _ := json.Marshal(rec)
	raw := "ticket-2"
	if err := os.WriteFile(g.ticketPath(raw), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.consumeTicket(raw, "alice.localhost:7658"); err == nil {
		t.Fatal("expired ticket accepted")
	}
}
