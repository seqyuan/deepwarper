package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/instance"
	"github.com/seqyuan/deepwarper/internal/model"
)

type Gateway struct {
	cfg       *config.Config
	mgr       *instance.Manager
	key       []byte
	ticketDir string
	logger    *log.Logger
	client    *http.Client
}

type ticketRecord struct {
	User       string    `json:"user"`
	InstanceID string    `json:"instanceId"`
	Authority  string    `json:"authority"`
	Exp        time.Time `json:"exp"`
}

type sessionPayload struct {
	User      string `json:"u"`
	Authority string `json:"a"`
	Exp       int64  `json:"e"`
	Version   int    `json:"v"`
}

func New(cfg *config.Config, mgr *instance.Manager, logger *log.Logger) (*Gateway, error) {
	key, err := loadOrCreateSecret(cfg.Platform.SessionFile)
	if err != nil {
		return nil, err
	}
	ticketDir := filepath.Join(cfg.Server.DataDir, "tickets")
	if err := os.MkdirAll(ticketDir, 0o700); err != nil {
		return nil, err
	}
	return &Gateway{
		cfg:       cfg,
		mgr:       mgr,
		key:       key,
		ticketDir: ticketDir,
		logger:    logger,
		client: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func loadOrCreateSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < 32 {
			return nil, fmt.Errorf("session secret %s is too short", path)
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return loadOrCreateSecret(path)
		}
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/platform/launch", g.handlePlatformLaunch)
	mux.HandleFunc("/api/local/status", g.handleStatus)
	mux.HandleFunc("/api/local/stop", g.handleStop)
	mux.HandleFunc("/_dw/launch", g.handleLaunchTicket)
	mux.HandleFunc("/login", g.handleLogin)
	mux.HandleFunc("/_dw/plugins", g.handlePlugins)
	mux.HandleFunc("/_dw/mounts", g.handleMounts)
	mux.HandleFunc("/", g.handleProxy)
	return mux
}

func (g *Gateway) handlePlatformLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		UserID       string `json:"user_id"`
		DisplayName  string `json:"display_name"`
		Distribution string `json:"distribution"`
		Workspace    string `json:"workspace"`
		Force        bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	inst, err := g.mgr.Ensure(r.Context(), instance.Request{
		User:         body.UserID,
		DisplayName:  body.DisplayName,
		Distribution: body.Distribution,
		Workspace:    body.Workspace,
		Force:        body.Force,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	t, err := g.issueTicket(inst)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u := fmt.Sprintf("%s://%s/_dw/launch?ticket=%s", g.cfg.Routing.PublicScheme, inst.PublicHost, t)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url":         u,
		"instance_id": inst.ID,
		"state":       string(inst.State),
	})
}

func (g *Gateway) issueTicket(inst *model.Instance) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw)
	rec := ticketRecord{
		User:       inst.User,
		InstanceID: inst.ID,
		Authority:  canonicalAuthority(inst.PublicHost),
		Exp:        time.Now().Add(g.cfg.TicketTTL()).UTC(),
	}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(g.ticketPath(t), data, 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func (g *Gateway) ticketPath(t string) string {
	sum := sha256.Sum256([]byte(t))
	return filepath.Join(g.ticketDir, hex.EncodeToString(sum[:])+".json")
}

func (g *Gateway) consumeTicket(t, authority string) (*ticketRecord, error) {
	path := g.ticketPath(t)
	used := strings.TrimSuffix(path, ".json") + ".used"
	f, err := os.OpenFile(used, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errors.New("ticket already used")
	}
	_ = f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("unknown ticket")
	}
	var rec ticketRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, errors.New("invalid ticket")
	}
	if time.Now().After(rec.Exp) {
		return nil, errors.New("ticket expired")
	}
	if rec.Authority != authority {
		return nil, errors.New("ticket authority mismatch")
	}
	_ = os.Remove(path)
	return &rec, nil
}

func (g *Gateway) handleLaunchTicket(w http.ResponseWriter, r *http.Request) {
	auth := canonicalAuthority(r.Host)
	rec, err := g.consumeTicket(r.URL.Query().Get("ticket"), auth)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	value, err := g.signSession(rec.User, auth)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "dw_session",
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   g.cfg.Routing.PublicScheme == "https",
		MaxAge:   int(g.cfg.SessionTTL().Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (g *Gateway) signSession(user, authority string) (string, error) {
	p := sessionPayload{User: user, Authority: authority, Exp: time.Now().Add(g.cfg.SessionTTL()).Unix()}
	if g.loginEnabled() {
		u, err := auth.Load(g.cfg.Auth.UsersDir, user)
		if err != nil {
			return "", err
		}
		p.Version = u.Auth.SessionVersion
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, g.key)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return "v1." + payload + "." + sig, nil
}

func (g *Gateway) verifySession(value, authority string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", false
	}
	mac := hmac.New(sha256.New, g.key)
	mac.Write([]byte(parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return "", false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var p sessionPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	if p.Exp < time.Now().Unix() || p.Authority != authority {
		return "", false
	}
	if g.loginEnabled() {
		u, err := auth.Load(g.cfg.Auth.UsersDir, p.User)
		if err != nil || u.Auth.SessionVersion != p.Version {
			return "", false
		}
	}
	return p.User, true
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	list, err := g.mgr.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, inst := range list {
		out = append(out, map[string]any{
			"id":           inst.ID,
			"user":         inst.User,
			"slug":         inst.Slug,
			"distribution": inst.Distribution,
			"state":        inst.State,
			"public_url":   fmt.Sprintf("%s://%s/", g.cfg.Routing.PublicScheme, inst.PublicHost),
			"home":         inst.Home,
			"workspace":    inst.Workspace,
			"port":         inst.Port,
			"error":        inst.Error,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (g *Gateway) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		UserID       string `json:"user_id"`
		Distribution string `json:"distribution"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Distribution == "" {
		body.Distribution = g.cfg.Runtime.DefaultDistribution
	}
	if err := g.mgr.Stop(r.Context(), body.UserID, body.Distribution); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) authorized(r *http.Request) bool {
	token := g.cfg.Platform.Token
	if token == "" && g.cfg.Platform.TokenFile != "" {
		if data, err := os.ReadFile(g.cfg.Platform.TokenFile); err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	want := "Bearer " + token
	got := r.Header.Get("Authorization")
	return token != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	inst, err := g.mgr.ByHost(r.Host)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if inst == nil {
		if g.loginEnabled() {
			g.handleUserHome(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	g.serveResolved(w, r, inst)
}

func (g *Gateway) serveResolved(w http.ResponseWriter, r *http.Request, inst *model.Instance) {
	auth := canonicalAuthority(r.Host)
	cookie, err := r.Cookie("dw_session")
	if err != nil {
		http.Error(w, "missing dw_session", http.StatusUnauthorized)
		return
	}
	user, ok := g.verifySession(cookie.Value, auth)
	if !ok {
		http.Error(w, "invalid dw_session", http.StatusUnauthorized)
		return
	}
	if user != inst.User {
		http.NotFound(w, r)
		return
	}
	if inst.State == model.StateStarting {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8">` +
			`<meta http-equiv="refresh" content="3">` +
			`<title>DeepWarper 启动中</title></head><body style="font-family:system-ui">` +
			`<h2>实例启动中</h2><p>正在等待本机进程就绪。本页每 3 秒自动刷新。</p>` +
			`<p>状态：` + string(inst.State) + `</p></body></html>`))
		return
	}
	if inst.State != model.StateRunning {
		http.Error(w, "instance is not running", http.StatusServiceUnavailable)
		return
	}
	dshCookie, err := g.ensureDshCookie(inst)
	if err != nil {
		http.Error(w, "dsh broker: "+err.Error(), http.StatusBadGateway)
		return
	}
	g.mgr.Touch(inst)

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", inst.Port)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = inst.PublicHost
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Cookie", dshCookie)
			pr.Out.Header.Set("Origin", g.cfg.Routing.PublicScheme+"://"+inst.PublicHost)
			pr.Out.Header.Set("X-Forwarded-Host", inst.PublicHost)
			pr.Out.Header.Set("X-Forwarded-Proto", g.cfg.Routing.PublicScheme)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "http://127.0.0.1:") {
				if u, err := url.Parse(loc); err == nil {
					u.Host = inst.PublicHost
					u.Scheme = g.cfg.Routing.PublicScheme
					resp.Header.Set("Location", u.String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.logger.Printf("proxy %s: %v", inst.ID, err)
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
		FlushInterval: 100 * time.Millisecond,
	}
	proxy.ServeHTTP(w, r)
}

func (g *Gateway) ensureDshCookie(inst *model.Instance) (string, error) {
	path := filepath.Join(g.cfg.Server.DataDir, "runtime", inst.ID, "dsh-session.json")
	if data, err := os.ReadFile(path); err == nil {
		var v struct {
			Cookie string `json:"cookie"`
		}
		if json.Unmarshal(data, &v) == nil && v.Cookie != "" {
			return v.Cookie, nil
		}
	}
	token, err := g.mgr.Token(inst)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", inst.Port, url.QueryEscape(token))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Host = inst.PublicHost
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	var cookie string
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, "dsh-auth-") {
			cookie = c.Name + "=" + c.Value
			break
		}
	}
	if cookie == "" {
		return "", fmt.Errorf("token exchange returned %d without dsh-auth cookie", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, _ := json.Marshal(map[string]string{"cookie": cookie})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return cookie, nil
}

func canonicalAuthority(host string) string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if hostOnly, port, err := net.SplitHostPort(h); err == nil {
		return hostOnly + ":" + port
	}
	return h
}
