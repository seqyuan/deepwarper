package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/instance"
	"github.com/seqyuan/deepwarper/internal/model"
)

func TestHostAllowlist(t *testing.T) {
	g := &Gateway{cfg: &config.Config{}}
	g.cfg.Routing.HostSuffix = "localhost"

	allowed := []string{
		"localhost",
		"localhost:7658",
		"alice.localhost",
		"Alice.LocalHost:7658",
		"alice.localhost.",
	}
	for _, host := range allowed {
		if !g.hostAllowed(normalizeHost(host)) {
			t.Errorf("expected allowed: %q", host)
		}
	}
	rejected := []string{
		"",
		"a.b.localhost",
		"evil.example",
		"user@localhost",
		"localhost/path",
		"local host",
		".localhost",
		"alice..localhost",
		"localhost:7658@evil",
		"localhost:notaport",
	}
	for _, host := range rejected {
		if g.hostAllowed(normalizeHost(host)) {
			t.Errorf("expected rejected: %q", host)
		}
	}

	g.cfg.Routing.HostSuffix = ""
	if g.hostAllowed("localhost") {
		t.Fatal("empty hostSuffix must reject everything")
	}
}

func TestEffectiveHostTrustsProxyOnlyWhenConfigured(t *testing.T) {
	g := &Gateway{cfg: &config.Config{}}
	g.cfg.Routing.HostSuffix = "localhost"
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "127.0.0.1:7658"
	r.Header.Set("X-Forwarded-Host", "Alice.Example.COM:443")

	if got := g.effectiveHost(r); got != "127.0.0.1:7658" {
		t.Fatalf("untrusted forwarded host used: %q", got)
	}
	g.cfg.Routing.TrustProxyHeaders = true
	if got := g.effectiveHost(r); got != "alice.example.com:443" {
		t.Fatalf("trusted forwarded host: %q", got)
	}
	r.Header.Set("X-Forwarded-Host", "first.example.com, second.example.com")
	if got := g.effectiveHost(r); got != "first.example.com" {
		t.Fatalf("first forwarded host: %q", got)
	}
}

func TestOriginAllowed(t *testing.T) {
	g := &Gateway{cfg: &config.Config{}}
	g.cfg.Routing.HostSuffix = "localhost"
	g.cfg.Routing.PublicScheme = "http"
	g.cfg.Routing.PublicPort = 7658

	cases := []struct {
		name    string
		host    string
		origin  string
		fetch   string
		allowed bool
	}{
		{name: "same origin", host: "alice.localhost:7658", origin: "http://alice.localhost:7658", allowed: true},
		{name: "missing origin", host: "alice.localhost:7658", allowed: true},
		{name: "cross-site without origin", host: "alice.localhost:7658", fetch: "cross-site", allowed: false},
		{name: "other host", host: "alice.localhost:7658", origin: "http://evil.example:7658", allowed: false},
		{name: "wrong port", host: "alice.localhost:7658", origin: "http://alice.localhost", allowed: false},
		{name: "wrong scheme", host: "alice.localhost:7658", origin: "https://alice.localhost:7658", allowed: false},
		{name: "opaque origin", host: "alice.localhost:7658", origin: "null", allowed: false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.fetch != "" {
			r.Header.Set("Sec-Fetch-Site", tc.fetch)
		}
		if got := g.originAllowed(r); got != tc.allowed {
			t.Errorf("%s: originAllowed = %v, want %v", tc.name, got, tc.allowed)
		}
	}

	// HTTPS behind a trusted proxy: the browser origin has no explicit port
	// while the loopback Host does. This used to be rejected as a port
	// mismatch.
	proxy := &Gateway{cfg: &config.Config{}}
	proxy.cfg.Routing.HostSuffix = "example.com"
	proxy.cfg.Routing.PublicScheme = "http"
	proxy.cfg.Routing.TrustProxyHeaders = true
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.Host = "127.0.0.1:7658"
	r.Header.Set("X-Forwarded-Host", "alice.example.com")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Port", "443")
	r.Header.Set("Origin", "https://alice.example.com")
	if !proxy.originAllowed(r) {
		t.Fatal("https origin behind a trusted proxy was rejected")
	}
	r.Header.Set("Origin", "https://alice.example.com:443")
	if !proxy.originAllowed(r) {
		t.Fatal("explicit default https port was rejected")
	}
	r.Header.Set("Origin", "https://alice.example.com:8443")
	if proxy.originAllowed(r) {
		t.Fatal("non-default port was accepted")
	}

	// Public port 80: the browser omits it entirely.
	plain := &Gateway{cfg: &config.Config{}}
	plain.cfg.Routing.HostSuffix = "example.com"
	plain.cfg.Routing.PublicScheme = "http"
	plain.cfg.Routing.PublicPort = 80
	r2 := httptest.NewRequest(http.MethodPost, "/login", nil)
	r2.Host = "alice.example.com"
	r2.Header.Set("Origin", "http://alice.example.com")
	if !plain.originAllowed(r2) {
		t.Fatal("port 80 origin was rejected")
	}
}

func TestConsumeUnknownOrInvalidTicketCreatesNoUsed(t *testing.T) {
	g := testGateway(t)
	for _, ticket := range []string{"", "does-not-exist", "../../etc/passwd"} {
		if _, err := g.consumeTicket(ticket, "alice.localhost:7658"); err == nil {
			t.Fatalf("unknown ticket %q accepted", ticket)
		}
	}
	entries, err := os.ReadDir(g.ticketDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unknown tickets created files: %v", entries)
	}
}

func TestInvalidExpiredAndMismatchedTicketsCreateNoUsed(t *testing.T) {
	g := testGateway(t)
	write := func(name string, rec ticketRecord) string {
		t.Helper()
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(g.ticketPath(name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(g.ticketPath(name), ".json") + ".used"
	}

	expiredUsed := write("expired", ticketRecord{User: "alice", Authority: "alice.localhost:7658", Exp: time.Now().Add(-time.Minute)})
	if _, err := g.consumeTicket("expired", "alice.localhost:7658"); err == nil {
		t.Fatal("expired ticket accepted")
	}
	mismatchUsed := write("mismatch", ticketRecord{User: "alice", Authority: "bob.localhost:7658", Exp: time.Now().Add(time.Minute)})
	if _, err := g.consumeTicket("mismatch", "alice.localhost:7658"); err == nil {
		t.Fatal("authority mismatch accepted")
	}
	if err := os.WriteFile(g.ticketPath("garbage"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	garbageUsed := strings.TrimSuffix(g.ticketPath("garbage"), ".json") + ".used"
	if _, err := g.consumeTicket("garbage", "alice.localhost:7658"); err == nil {
		t.Fatal("garbage ticket accepted")
	}
	for _, used := range []string{expiredUsed, mismatchUsed, garbageUsed} {
		if _, err := os.Stat(used); !os.IsNotExist(err) {
			t.Fatalf("rejected ticket created %s", used)
		}
	}

	// A valid ticket still claims the marker exactly once.
	write("good", ticketRecord{User: "alice", Authority: "alice.localhost:7658", Exp: time.Now().Add(time.Minute)})
	if _, err := g.consumeTicket("good", "alice.localhost:7658"); err != nil {
		t.Fatal(err)
	}
	goodUsed := strings.TrimSuffix(g.ticketPath("good"), ".json") + ".used"
	if _, err := os.Stat(goodUsed); err != nil {
		t.Fatalf("valid ticket did not claim a marker: %v", err)
	}
}

func TestIssueTicketAtomicAndSingleUse(t *testing.T) {
	g := testGateway(t)
	inst := &model.Instance{User: "alice", ID: "alice-default", PublicHost: "alice.localhost:7658"}
	ticket, err := g.issueTicket(inst)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(g.ticketPath(ticket))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("ticket mode: %o", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(g.ticketDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), atomicTempPattern) {
			t.Fatalf("issue ticket left a temp file: %s", e.Name())
		}
	}
	if _, err := g.consumeTicket(ticket, "alice.localhost:7658"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.consumeTicket(ticket, "alice.localhost:7658"); err == nil {
		t.Fatal("ticket consumed twice")
	}
}

func TestCleanupTickets(t *testing.T) {
	g := testGateway(t)
	now := time.Now()
	expired := ticketRecord{User: "alice", Authority: "alice.localhost:7658", Exp: now.Add(-5 * time.Minute)}
	valid := ticketRecord{User: "alice", Authority: "alice.localhost:7658", Exp: now.Add(5 * time.Minute)}
	for name, rec := range map[string]ticketRecord{"expired": expired, "valid": valid} {
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(g.ticketPath(name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A consumed marker must outlive the 24h maximum ticket TTL, otherwise a
	// ticket whose .json removal failed could be replayed after pruning.
	maxTTLUsed := filepath.Join(g.ticketDir, "maxttl.used")
	oldUsed := filepath.Join(g.ticketDir, "old.used")
	freshUsed := filepath.Join(g.ticketDir, "fresh.used")
	oldTemp := filepath.Join(g.ticketDir, atomicTempPattern+"old")
	freshTemp := filepath.Join(g.ticketDir, atomicTempPattern+"fresh")
	for _, p := range []string{maxTTLUsed, oldUsed, freshUsed, oldTemp, freshTemp} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	maxTTL := now.Add(-24 * time.Hour)
	if err := os.Chtimes(maxTTLUsed, maxTTL, maxTTL); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-g.ticketUsedRetention() - time.Hour)
	for _, p := range []string{oldUsed, oldTemp} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.cleanupTickets(now); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{g.ticketPath("expired"), oldUsed, oldTemp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("janitor did not remove %s", p)
		}
	}
	for _, p := range []string{g.ticketPath("valid"), freshUsed, freshTemp, maxTTLUsed} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("janitor removed live %s: %v", p, err)
		}
	}
}

func TestCleanupTicketsUsedMarkerOutlivesMaxTicketTTL(t *testing.T) {
	g := testGateway(t)
	g.cfg.Platform.TicketTTL = "24h"
	if retention := g.ticketUsedRetention(); retention <= 24*time.Hour {
		t.Fatalf("retention %s does not exceed the maximum ticket TTL", retention)
	}
	now := time.Now()
	marker := filepath.Join(g.ticketDir, "consumed.used")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Exactly at the maximum ticket TTL the ticket could still be parseable, so
	// the marker must survive.
	at := now.Add(-24 * time.Hour)
	if err := os.Chtimes(marker, at, at); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupTickets(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf(".used marker pruned before the maximum ticket TTL: %v", err)
	}

	// Beyond the retention window the marker is pruned.
	stale := now.Add(-g.ticketUsedRetention() - time.Minute)
	if err := os.Chtimes(marker, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupTickets(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale .used marker survived: %v", err)
	}
}

func TestRunTicketJanitorUntilCanceled(t *testing.T) {
	g := testGateway(t)
	data, _ := json.Marshal(ticketRecord{User: "alice", Authority: "alice.localhost:7658", Exp: time.Now().Add(-5 * time.Minute)})
	if err := os.WriteFile(g.ticketPath("expired"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.RunTicketJanitor(ctx, 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(g.ticketPath("expired")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("janitor did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
}

func TestLaunchTicketHostAllowlistAndHeaders(t *testing.T) {
	g := testGateway(t)
	g.cfg.Routing.HostSuffix = "localhost"
	g.cfg.Routing.PublicScheme = "http"
	g.cfg.Platform.SessionTTL = "1h"
	inst := &model.Instance{User: "alice", ID: "alice-default", PublicHost: "alice.localhost:7658"}
	ticket, err := g.issueTicket(inst)
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler()

	rr := doReq(t, h, http.MethodGet, "/_dw/launch?ticket="+ticket, "evil.example:7658", nil, nil)
	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("disallowed host: %d", rr.Code)
	}
	// The host rejection must not have consumed the ticket.
	rr = doReq(t, h, http.MethodGet, "/_dw/launch?ticket="+ticket, "alice.localhost:7658", nil, nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("valid launch: %d %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control: %q", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy: %q", got)
	}
	if rr.Header().Get("Location") != "/" {
		t.Fatalf("Location: %q", rr.Header().Get("Location"))
	}
}

func TestHealthzAndBearerAPISkipHostAllowlist(t *testing.T) {
	g, _ := loginGateway(t, "")
	g.cfg.Platform.Token = "tok"
	h := g.Handler()

	rr := doReq(t, h, http.MethodGet, "/healthz", "127.0.0.1:9", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rr.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/local/status", nil)
	req.Host = "127.0.0.1:9"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer api: %d %s", rec.Code, rec.Body.String())
	}
	// Without a bearer credential the same route is host-checked.
	rr = doReq(t, h, http.MethodGet, "/api/local/status", "127.0.0.1:9", nil, nil)
	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("anonymous api: %d", rr.Code)
	}
}

func TestWriteLaunchErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		code  int
		retry bool
	}{
		{name: "invalid", err: fmt.Errorf("%w: nope", instance.ErrInvalidRequest), code: http.StatusBadRequest},
		{name: "unknown user", err: fmt.Errorf("%w: nope", instance.ErrUnknownUser), code: http.StatusUnauthorized},
		{name: "busy", err: &instance.BusyError{User: "alice", Distribution: "default", State: model.StateStopping}, code: http.StatusConflict, retry: true},
		{name: "other", err: errors.New("boom"), code: http.StatusBadGateway},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		writeLaunchError(rr, tc.err)
		if rr.Code != tc.code {
			t.Errorf("%s: code %d, want %d", tc.name, rr.Code, tc.code)
		}
		if tc.retry && rr.Header().Get("Retry-After") == "" {
			t.Errorf("%s: missing Retry-After", tc.name)
		}
	}
}

func TestStopErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{name: "unknown instance", err: fmt.Errorf("%w: alice/default", instance.ErrUnknownInstance), code: http.StatusNotFound},
		{name: "invalid request", err: fmt.Errorf("%w: nope", instance.ErrInvalidRequest), code: http.StatusBadRequest},
		{name: "unknown user", err: fmt.Errorf("%w: nope", instance.ErrUnknownUser), code: http.StatusUnauthorized},
		{name: "other", err: errors.New("boom"), code: http.StatusBadGateway},
	}
	for _, tc := range cases {
		if got := stopErrorStatus(tc.err); got != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.code)
		}
	}
}

func TestCanonicalAuthorityNormalizesDefaultPort(t *testing.T) {
	cases := map[string]string{
		"alice.localhost":       "alice.localhost",
		"Alice.LocalHost.:443":  "alice.localhost",
		"alice.localhost:443":   "alice.localhost",
		"alice.localhost:80":    "alice.localhost",
		"alice.localhost:7658":  "alice.localhost:7658",
		"alice.localhost:08080": "alice.localhost:08080",
	}
	for host, want := range cases {
		if got := canonicalAuthority(host); got != want {
			t.Errorf("canonicalAuthority(%q) = %q, want %q", host, got, want)
		}
	}
	g := testGateway(t)
	value, err := g.signSession("alice", canonicalAuthority("alice.localhost:443"))
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := g.verifySession(value, canonicalAuthority("alice.localhost")); !ok || user != "alice" {
		t.Fatalf("default-port ticket did not verify on the bare host: %q %v", user, ok)
	}
	if user, ok := g.verifySession(value, canonicalAuthority("alice.localhost:443")); !ok || user != "alice" {
		t.Fatalf("default-port ticket did not verify on host:443: %q %v", user, ok)
	}
}

func TestSessionInvalidAfterDeleteAndRecreate(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	if err := auth.Create(users, "alice", "first-secret"); err != nil {
		t.Fatal(err)
	}
	gw, _ := loginGateway(t, users)
	const host = "alice.localhost:7658"
	value, err := gw.signSession("alice", host)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.verifySession(value, host); !ok {
		t.Fatal("fresh session did not verify")
	}
	if err := auth.Delete(users, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.verifySession(value, host); ok {
		t.Fatal("session survived deletion")
	}
	if _, err := gw.signSession("alice", host); !errors.Is(err, errUserUnavailable) {
		t.Fatalf("signSession for a deleted user: %v", err)
	}
	if err := auth.Create(users, "alice", "second-secret"); err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.verifySession(value, host); ok {
		t.Fatal("old session revived after recreate")
	}
	fresh, err := gw.signSession("alice", host)
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := gw.verifySession(fresh, host); !ok || user != "alice" {
		t.Fatalf("new session: %q %v", user, ok)
	}
}

func TestDeletedUserTicketReturns401(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	if err := auth.Create(users, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	gw, _ := loginGateway(t, users)
	inst := &model.Instance{User: "alice", ID: "alice-default", PublicHost: "alice.localhost:7658"}
	ticket, err := gw.issueTicket(inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Delete(users, "alice"); err != nil {
		t.Fatal(err)
	}
	rr := doReq(t, gw.Handler(), http.MethodGet, "/_dw/launch?ticket="+ticket, "alice.localhost:7658", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted-user ticket: %d %s", rr.Code, rr.Body.String())
	}
}

func saveBrokerInstance(t *testing.T, mgr *instance.Manager, dataDir string, port int) *model.Instance {
	t.Helper()
	control := filepath.Join(dataDir, "control", "alice-default")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "token"), []byte("dsh-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := &model.Instance{
		ID:           "alice-default",
		User:         "alice",
		Slug:         "alice",
		Distribution: "default",
		State:        model.StateRunning,
		Incarnation:  "inc-1",
		PublicHost:   "alice.localhost:7658",
		Port:         port,
		Home:         filepath.Join(dataDir, "homes", "alice"),
		Workspace:    filepath.Join(dataDir, "workspaces", "alice"),
		ControlDir:   control,
	}
	if err := mgr.Store().Save(inst); err != nil {
		t.Fatal(err)
	}
	return inst
}
func TestProxyStripsUpstreamSetCookie(t *testing.T) {
	var mu sync.Mutex
	issued := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "" {
			mu.Lock()
			issued = "cookie-1"
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-1", Value: "cookie-1", Path: "/"})
			return
		}
		mu.Lock()
		ok := issued != ""
		mu.Unlock()
		if !ok {
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		// A hostile upstream Set-Cookie must never reach the browser.
		http.SetCookie(w, &http.Cookie{Name: "leak", Value: "1", Path: "/"})
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	port := serverPort(t, upstream)

	gw, mgr := loginGateway(t, "")
	inst := saveBrokerInstance(t, mgr, gw.cfg.Server.DataDir, port)

	value, err := gw.signSession(inst.User, "alice.localhost:7658")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "alice.localhost:7658"
	req.AddCookie(&http.Cookie{Name: "dw_session", Value: value})
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Result().Cookies(); len(got) != 0 {
		t.Fatalf("upstream Set-Cookie leaked: %v", got)
	}
	cached, ok := gw.readDshCookie(inst)
	if !ok || cached != "dsh-auth-1=cookie-1" {
		t.Fatalf("cached dsh cookie: %q %v", cached, ok)
	}
}

func TestDshCookieRefreshOn401AndFresh401Is502(t *testing.T) {
	var mu sync.Mutex
	issued := ""
	rejectAll := false
	var pageCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "" {
			mu.Lock()
			issued = fmt.Sprintf("cookie-%d", time.Now().UnixNano())
			value := issued
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-1", Value: value, Path: "/"})
			return
		}
		mu.Lock()
		pageCalls++
		reject := rejectAll
		want := issued
		mu.Unlock()
		got := ""
		if c, err := r.Cookie("dsh-auth-1"); err == nil {
			got = c.Value
		}
		if reject || want == "" || got != want {
			http.Error(w, "stale cookie", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	port := serverPort(t, upstream)

	gw, mgr := loginGateway(t, "")
	inst := saveBrokerInstance(t, mgr, gw.cfg.Server.DataDir, port)

	value, err := gw.signSession(inst.User, "alice.localhost:7658")
	if err != nil {
		t.Fatal(err)
	}
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "alice.localhost:7658"
		req.AddCookie(&http.Cookie{Name: "dw_session", Value: value})
		rec := httptest.NewRecorder()
		gw.Handler().ServeHTTP(rec, req)
		return rec
	}

	// First request: token exchange + page.
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("initial: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	pageCalls = 0
	// Simulate a dsh restart: the cached cookie is now stale.
	issued = ""
	mu.Unlock()

	// Cached cookie 401 -> invalidate, re-exchange, retry once, succeed.
	rec := do()
	if rec.Code != http.StatusOK {
		t.Fatalf("after refresh: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	calls := pageCalls
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected one rejected retry and one success, got %d page calls", calls)
	}

	// A freshly exchanged cookie rejected by upstream is a 502 with no loop.
	refreshed, ok := gw.readDshCookie(inst)
	if !ok {
		t.Fatal("refresh did not cache a cookie")
	}
	mu.Lock()
	rejectAll = true
	pageCalls = 0
	mu.Unlock()
	rec = do()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("fresh 401: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	calls = pageCalls
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("fresh 401 must not loop: %d page calls", calls)
	}
	if current, ok := gw.readDshCookie(inst); ok && current == refreshed {
		t.Fatal("stale cookie stayed cached after the refresh attempt")
	}
}

func TestCached401OnPostIsForwardedWithoutReplay(t *testing.T) {
	const body = `{"hello":"world"}`
	var mu sync.Mutex
	var pageCalls int
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "" {
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-1", Value: "cookie-1", Path: "/"})
			return
		}
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		pageCalls++
		seen = string(data)
		mu.Unlock()
		http.Error(w, "stale cookie", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	gw, mgr := loginGateway(t, "")
	inst := saveBrokerInstance(t, mgr, gw.cfg.Server.DataDir, serverPort(t, upstream))
	// Prime the cache with a cookie the upstream rejects.
	if err := gw.writeDshCookie(inst, "dsh-auth-1=cookie-1"); err != nil {
		t.Fatal(err)
	}
	value, err := gw.signSession(inst.User, "alice.localhost:7658")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(body))
	req.Host = "alice.localhost:7658"
	req.AddCookie(&http.Cookie{Name: "dw_session", Value: value})
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	// A consumed request body cannot be replayed, so the cached 401 is
	// forwarded instead of being retried (which would be a 502/body error).
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cached 401 on POST: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	calls, got := pageCalls, seen
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream page calls: %d, want 1", calls)
	}
	if got != body {
		t.Fatalf("upstream body: %q, want %q", got, body)
	}
}

func TestIdempotentRequestRequiresBodylessRequest(t *testing.T) {
	body := func(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
	cases := []struct {
		name    string
		resp    *http.Response
		allowed bool
	}{
		{name: "nil response", resp: nil, allowed: true},
		{name: "GET", resp: &http.Response{Request: &http.Request{Method: http.MethodGet, Body: http.NoBody}}, allowed: true},
		{name: "HEAD", resp: &http.Response{Request: &http.Request{Method: http.MethodHead, Body: http.NoBody}}, allowed: true},
		{name: "OPTIONS", resp: &http.Response{Request: &http.Request{Method: http.MethodOptions, Body: http.NoBody}}, allowed: true},
		{name: "GET with body", resp: &http.Response{Request: &http.Request{Method: http.MethodGet, Body: body("x"), ContentLength: 1}}, allowed: false},
		{name: "GET with chunked body", resp: &http.Response{Request: &http.Request{Method: http.MethodGet, Body: body("x"), ContentLength: -1}}, allowed: false},
		{name: "POST without body", resp: &http.Response{Request: &http.Request{Method: http.MethodPost, Body: http.NoBody}}, allowed: false},
		{name: "DELETE without body", resp: &http.Response{Request: &http.Request{Method: http.MethodDelete, Body: http.NoBody}}, allowed: false},
	}
	for _, tc := range cases {
		if got := idempotentRequest(tc.resp); got != tc.allowed {
			t.Errorf("%s: idempotentRequest = %v, want %v", tc.name, got, tc.allowed)
		}
	}
}

func TestCached401OnGetWithBodyIsForwardedWithoutReplay(t *testing.T) {
	const body = `{"hello":"world"}`
	var mu sync.Mutex
	var pageCalls int
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "" {
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-1", Value: "cookie-1", Path: "/"})
			return
		}
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		pageCalls++
		seen = string(data)
		mu.Unlock()
		http.Error(w, "stale cookie", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	gw, mgr := loginGateway(t, "")
	inst := saveBrokerInstance(t, mgr, gw.cfg.Server.DataDir, serverPort(t, upstream))
	if err := gw.writeDshCookie(inst, "dsh-auth-1=cookie-1"); err != nil {
		t.Fatal(err)
	}
	value, err := gw.signSession(inst.User, "alice.localhost:7658")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/query", strings.NewReader(body))
	req.Host = "alice.localhost:7658"
	req.AddCookie(&http.Cookie{Name: "dw_session", Value: value})
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	// A GET with a body cannot be replayed either: the body was consumed by
	// the first attempt, so the 401 is forwarded instead of retried.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cached 401 on GET with body: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	calls, got := pageCalls, seen
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream page calls: %d, want 1", calls)
	}
	if got != body {
		t.Fatalf("upstream body: %q, want %q", got, body)
	}
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestDshCookieCacheIgnoresOldFormatAndOtherIncarnations(t *testing.T) {
	gw, mgr := loginGateway(t, "")
	inst := saveBrokerInstance(t, mgr, gw.cfg.Server.DataDir, 1)
	path, err := gw.dshCookiePath(inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"cookie":"dsh-auth-1=old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.readDshCookie(inst); ok {
		t.Fatal("old one-string cache format was accepted")
	}
	if err := gw.writeDshCookie(inst, "dsh-auth-1=new"); err != nil {
		t.Fatal(err)
	}
	if got, ok := gw.readDshCookie(inst); !ok || got != "dsh-auth-1=new" {
		t.Fatalf("fresh cache: %q %v", got, ok)
	}
	other := *inst
	other.Incarnation = "inc-2"
	if _, ok := gw.readDshCookie(&other); ok {
		t.Fatal("cache from another incarnation was accepted")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode: %o", fi.Mode().Perm())
	}
	if _, err := gw.dshCookiePath(&model.Instance{ID: "../escape"}); err == nil {
		t.Fatal("unsafe instance id accepted")
	}
}

func TestServeResolvedPendingAndStoppingReturn202(t *testing.T) {
	gw, mgr := loginGateway(t, "")
	inst := &model.Instance{
		ID: "alice-default", User: "alice", Slug: "alice", Distribution: "default",
		State: model.StatePending, PublicHost: "alice.localhost:7658", Port: 1,
		Home: filepath.Join(t.TempDir(), "home"), Workspace: filepath.Join(t.TempDir(), "ws"),
		ControlDir: filepath.Join(t.TempDir(), "control"),
	}
	if err := mgr.Store().Save(inst); err != nil {
		t.Fatal(err)
	}
	value, err := gw.signSession("alice", "alice.localhost:7658")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "dw_session", Value: value}
	h := gw.Handler()
	for _, state := range []model.State{model.StatePending, model.StateStarting, model.StateStopping} {
		inst.State = state
		if err := mgr.Store().Save(inst); err != nil {
			t.Fatal(err)
		}
		rr := doReq(t, h, http.MethodGet, "/", "alice.localhost:7658", nil, cookie)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", state, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: missing Retry-After", state)
		}
	}
}

func TestLoginApexRedirectsToInstanceTicket(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	if err := auth.Create(users, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	gw, mgr := loginGateway(t, users)
	inst := &model.Instance{
		ID: "alice-default", User: "alice", Slug: "alice", Distribution: "default",
		State: model.StateRunning, PublicHost: "alice.localhost:7658", Port: 1,
		Home: filepath.Join(root, "home"), Workspace: filepath.Join(root, "ws"),
		ControlDir: filepath.Join(root, "control"),
	}
	if err := mgr.Store().Save(inst); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()

	rr := doReq(t, h, http.MethodPost, "/login", "localhost:7658", url.Values{"username": {"alice"}, "password": {"secret"}}, nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("apex login: %d %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			t.Fatal("apex login set a host-only cookie instead of redirecting")
		}
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://alice.localhost:7658/_dw/launch?ticket=") {
		t.Fatalf("redirect: %q", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	ticket := u.Query().Get("ticket")
	if ticket == "" {
		t.Fatal("redirect has no ticket")
	}

	rr = doReq(t, h, http.MethodGet, "/_dw/launch?ticket="+ticket, "alice.localhost:7658", nil, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("launch: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("launch did not set dw_session")
	}
	if user, ok := gw.verifySession(cookie.Value, "alice.localhost:7658"); !ok || user != "alice" {
		t.Fatalf("launch session: %q %v", user, ok)
	}
}

func TestLoginOnInstanceSubdomainKeepsCookie(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	if err := auth.Create(users, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	gw, _ := loginGateway(t, users)
	rr := doReq(t, gw.Handler(), http.MethodPost, "/login", "alice.localhost:7658", url.Values{"username": {"alice"}, "password": {"secret"}}, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("subdomain login: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("subdomain login did not set dw_session")
	}
	if user, ok := gw.verifySession(cookie.Value, "alice.localhost:7658"); !ok || user != "alice" {
		t.Fatalf("subdomain session: %q %v", user, ok)
	}
}

func TestLoginRateLimitedAfterFailures(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	if err := auth.Create(users, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	gw, _ := loginGateway(t, users)
	gw.limiter = newLoginLimiter(2, time.Minute, time.Minute)
	h := gw.Handler()
	host := "localhost:7658"

	for i := 0; i < 2; i++ {
		rr := doReq(t, h, http.MethodPost, "/login", host, url.Values{"username": {"alice"}, "password": {"wrong"}}, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rr.Code)
		}
	}
	rr := doReq(t, h, http.MethodPost, "/login", host, url.Values{"username": {"alice"}, "password": {"secret"}}, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	// GET /login is not limited.
	for i := 0; i < 3; i++ {
		if rr := doReq(t, h, http.MethodGet, "/login", host, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("login GET %d: %d", i, rr.Code)
		}
	}
}

func TestLoginLimiterResetOnSuccessAndPrune(t *testing.T) {
	l := newLoginLimiter(2, time.Minute, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	key := "203.0.113.9|alice"
	l.fail(key)
	l.fail(key)
	if ok, wait := l.allow(key); ok || wait <= 0 {
		t.Fatalf("limiter did not block: %v %v", ok, wait)
	}
	// The lockout expires and a new window starts.
	now = now.Add(2 * time.Minute)
	if ok, _ := l.allow(key); !ok {
		t.Fatal("limiter stayed blocked after lockout")
	}
	l.fail(key)
	l.reset(key)
	if ok, _ := l.allow(key); !ok {
		t.Fatal("reset did not clear the key")
	}
	l.fail(key)
	now = now.Add(10 * time.Minute)
	l.prune(now)
	l.mu.Lock()
	_, present := l.entries[key]
	l.mu.Unlock()
	if present {
		t.Fatal("prune kept an idle entry")
	}
}

func TestClientIPHonorsForwardedForOnlyWhenTrusted(t *testing.T) {
	g := &Gateway{cfg: &config.Config{}}
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.9")
	if got := g.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("untrusted XFF: %q", got)
	}
	g.cfg.Routing.TrustProxyHeaders = true
	if got := g.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("trusted XFF last hop: %q", got)
	}
}
