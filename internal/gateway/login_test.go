package gateway

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/instance"
	"github.com/seqyuan/deepwarper/internal/model"
)

func loginGateway(t *testing.T, usersDir string) (*Gateway, *instance.Manager) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.DataDir = filepath.Join(root, "data")
	cfg.Server.Listen = "127.0.0.1:7658"
	cfg.Routing.HostSuffix = "localhost"
	cfg.Routing.PublicScheme = "http"
	cfg.Platform.SessionTTL = "1h"
	cfg.Platform.TicketTTL = "60s"
	cfg.Platform.SessionFile = filepath.Join(root, "session")
	cfg.Runtime.Default = "local"
	cfg.Runtime.DefaultDistribution = "default"
	cfg.Runtime.DSHBin = "/bin/true"
	cfg.Runtime.DWBin = "/bin/true"
	cfg.Runtime.ReadyTimeout = "90s"
	cfg.Runtime.ScanInterval = "30s"
	cfg.Runtime.Heartbeat = "30s"
	cfg.Runtime.Local.PortRange = [2]int{26000, 26020}
	cfg.Runtime.Local.Limiter = "none"
	cfg.Runtime.Local.TasksMax = 512
	cfg.Runtime.Local.SystemdStartTimeout = "10s"
	cfg.Runtime.Local.StopGrace = "20s"
	cfg.Runtime.Local.UnitStdio = "log"
	cfg.Auth.UsersDir = usersDir
	mgr, err := instance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(cfg, mgr, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return gw, mgr
}

func doReq(t *testing.T, h http.Handler, method, target, host string, body url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(body.Encode())
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Host = host
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestLoginFlowMissingInstance(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "config", "users")
	if err := auth.Create(users, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	office := filepath.Join(root, "office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := "plugins:\n  - name: office\n    path: " + office + "\n  - name: sheets\n    path: " + office + "\n"
	if err := os.WriteFile(filepath.Join(root, "config", "catalog.yaml"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := auth.Load(users, "alice")
	if err != nil {
		t.Fatal(err)
	}
	u.Plugins = []string{"office"}
	u.Mounts = []auth.Mount{{Name: "ref", Kind: "path", Host: "/srv/data/ref", Target: "/data/ref", Mode: "ro"}}
	if err := auth.Save(users, u); err != nil {
		t.Fatal(err)
	}

	gw, mgr := loginGateway(t, users)
	h := gw.Handler()
	host := "localhost:7658"

	rr := doReq(t, h, http.MethodGet, "/healthz", host, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rr.Code)
	}
	rr = doReq(t, h, http.MethodGet, "/login", host, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("login form: %d %s", rr.Code, rr.Body.String())
	}
	form := rr.Body.String()
	if !strings.Contains(form, `name="username"`) || !strings.Contains(form, `name="password"`) || !strings.Contains(form, `action="/login"`) {
		t.Fatalf("form: %s", form)
	}

	rr = doReq(t, h, http.MethodGet, "/", host, nil, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous /: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	rr = doReq(t, h, http.MethodPost, "/login", host, url.Values{"username": {"alice"}, "password": {"nope"}}, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			t.Fatal("bad login set dw_session")
		}
	}

	rr = doReq(t, h, http.MethodPost, "/login", host, url.Values{"username": {"alice"}, "password": {"secret"}}, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("login: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("login did not set dw_session")
	}

	rr = doReq(t, h, http.MethodGet, "/", host, nil, cookie)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "未运行") {
		t.Fatalf("missing instance: %d %s", rr.Code, rr.Body.String())
	}

	rr = doReq(t, h, http.MethodGet, "/_dw/plugins", host, nil, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("plugins anon: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	rr = doReq(t, h, http.MethodGet, "/_dw/mounts", host, nil, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("mounts anon: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	rr = doReq(t, h, http.MethodGet, "/_dw/plugins", host, nil, cookie)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "office") || !strings.Contains(rr.Body.String(), "已启用") || !strings.Contains(rr.Body.String(), "sheets") {
		t.Fatalf("plugins: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, h, http.MethodGet, "/_dw/mounts", host, nil, cookie)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/data/ref") {
		t.Fatalf("mounts: %d %s", rr.Code, rr.Body.String())
	}

	inst := &model.Instance{
		ID: "alice-default", User: "alice", Slug: "alice", Distribution: "default",
		State: model.StateRunning, PublicHost: "alice.localhost:7658", Port: 1,
		Home: filepath.Join(root, "home"), Workspace: filepath.Join(root, "ws"),
		ControlDir: filepath.Join(root, "control"),
	}
	if err := mgr.Store().Save(inst); err != nil {
		t.Fatal(err)
	}
	rr = doReq(t, h, http.MethodGet, "/", "alice.localhost:7658", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("subdomain without cookie: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, h, http.MethodGet, "/", host, nil, cookie)
	if rr.Code == http.StatusServiceUnavailable && strings.Contains(rr.Body.String(), "未运行") {
		t.Fatal("running instance was reported as not running")
	}
}

func TestLoginDisabledKeepsOldRoot(t *testing.T) {
	gw, _ := loginGateway(t, "")
	h := gw.Handler()
	rr := doReq(t, h, http.MethodGet, "/login", "localhost:7658", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("login: %d", rr.Code)
	}
	rr = doReq(t, h, http.MethodGet, "/", "localhost:7658", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/: %d", rr.Code)
	}
	rr = doReq(t, h, http.MethodGet, "/_dw/plugins", "localhost:7658", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("plugins: %d", rr.Code)
	}
}

func TestLoginOriginAndSessionVersion(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	if err := auth.Create(users, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	gw, _ := loginGateway(t, users)
	h := gw.Handler()
	host := "localhost:7658"
	body := url.Values{"username": {"alice"}, "password": {"secret"}}

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			t.Fatal("foreign origin set a cookie")
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:7658")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("matching origin: %d %s", rr.Code, rr.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login did not set a cookie")
	}
	if user, ok := gw.verifySession(cookie.Value, host); !ok || user != "alice" {
		t.Fatalf("fresh cookie: %q %v", user, ok)
	}
	if err := auth.SetPassword(users, "alice", "next"); err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.verifySession(cookie.Value, host); ok {
		t.Fatal("cookie survived a password change")
	}
	body.Set("password", "next")
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	cookie = nil
	for _, c := range rr.Result().Cookies() {
		if c.Name == "dw_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login after password change did not set a cookie")
	}
	if err := auth.Delete(users, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.verifySession(cookie.Value, host); ok {
		t.Fatal("cookie survived user deletion")
	}
}
