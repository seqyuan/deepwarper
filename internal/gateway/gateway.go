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
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	limiterOnce sync.Once
	limiter     *loginLimiter

	dshMu    sync.Mutex
	dshLocks map[string]*sync.Mutex
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
	max, window, lockout := cfg.LoginLimit()
	return &Gateway{
		cfg:       cfg,
		mgr:       mgr,
		key:       key,
		ticketDir: ticketDir,
		logger:    logger,
		limiter:   newLoginLimiter(max, window, lockout),
		dshLocks:  map[string]*sync.Mutex{},
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
	// Host allowlist for every browser-facing route. A request for an
	// unrelated Host is a DNS-rebinding/login-CSRF attempt, not a typo.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requiresHostCheck(r) && !g.hostAllowed(g.effectiveHost(r)) {
			http.Error(w, "unexpected host", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
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
		writeLaunchError(w, err)
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

func writeLaunchError(w http.ResponseWriter, err error) {
	var busy *instance.BusyError
	switch {
	case errors.As(err, &busy), errors.Is(err, instance.ErrBusy):
		w.Header().Set("Retry-After", "5")
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, instance.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, instance.ErrUnknownUser):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	default:
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
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
	data, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(g.ticketDir, g.ticketPath(t), data, 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func (g *Gateway) ticketPath(t string) string {
	sum := sha256.Sum256([]byte(t))
	return filepath.Join(g.ticketDir, hex.EncodeToString(sum[:])+".json")
}

// consumeTicket validates a ticket before claiming it. Unknown, malformed,
// expired and mismatched tickets therefore create no .used inode: an
// unauthenticated /_dw/launch?ticket=random cannot fill the ticket directory.
func (g *Gateway) consumeTicket(t, authority string) (*ticketRecord, error) {
	if t == "" {
		return nil, errors.New("unknown ticket")
	}
	path := g.ticketPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("unknown ticket")
	}
	var rec ticketRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, errors.New("invalid ticket")
	}
	if !rec.Exp.After(time.Now()) {
		return nil, errors.New("ticket expired")
	}
	if rec.Authority != authority {
		return nil, errors.New("ticket authority mismatch")
	}
	used := strings.TrimSuffix(path, ".json") + ".used"
	f, err := os.OpenFile(used, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, errors.New("ticket already used")
		}
		return nil, errors.New("ticket unavailable")
	}
	_ = f.Close()
	_ = os.Remove(path)
	return &rec, nil
}

func (g *Gateway) handleLaunchTicket(w http.ResponseWriter, r *http.Request) {
	// A ticket is a bearer secret in the URL: never cache the response and
	// never leak the URL through Referer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	authority := canonicalAuthority(g.effectiveHost(r))
	rec, err := g.consumeTicket(r.URL.Query().Get("ticket"), authority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	value, err := g.signSession(rec.User, authority)
	if err != nil {
		if errors.Is(err, errUserUnavailable) {
			http.Error(w, "unknown user", http.StatusUnauthorized)
			return
		}
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "dw_session",
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   g.requestScheme(r) == "https",
		MaxAge:   int(g.cfg.SessionTTL().Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// errUserUnavailable means the cookie/ticket names an account that was deleted
// (tombstoned) or disappeared. It maps to 401, not 500.
var errUserUnavailable = errors.New("user is unavailable")

func (g *Gateway) signSession(user, authority string) (string, error) {
	p := sessionPayload{User: user, Authority: authority, Exp: time.Now().Add(g.cfg.SessionTTL()).Unix()}
	if g.loginEnabled() {
		u, err := auth.Load(g.cfg.Auth.UsersDir, user)
		if err != nil {
			if os.IsNotExist(err) {
				return "", errUserUnavailable
			}
			return "", err
		}
		if !u.Active() {
			return "", errUserUnavailable
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
		if err != nil || !u.Active() || u.Auth.SessionVersion != p.Version {
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
		http.Error(w, err.Error(), stopErrorStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stopErrorStatus maps a lifecycle error to its HTTP status. Busy is not part
// of Stop's contract (a Stopping record is stopped, not rejected).
func stopErrorStatus(err error) int {
	switch {
	case errors.Is(err, instance.ErrUnknownInstance):
		return http.StatusNotFound
	case errors.Is(err, instance.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, instance.ErrUnknownUser):
		return http.StatusUnauthorized
	default:
		return http.StatusBadGateway
	}
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
	host := g.effectiveHost(r)
	inst, err := g.mgr.ByHost(host)
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

// errRefreshDshCookie and errFreshDshCookie are the two proxy sentinels: the
// first asks serveResolved to invalidate the cached cookie and retry once; the
// second means a freshly exchanged cookie was rejected too (502, no loop).
var (
	errRefreshDshCookie = errors.New("dsh cookie needs a refresh")
	errFreshDshCookie   = errors.New("dsh rejected a fresh cookie")
)

func (g *Gateway) serveResolved(w http.ResponseWriter, r *http.Request, inst *model.Instance) {
	authority := canonicalAuthority(g.effectiveHost(r))
	cookie, err := r.Cookie("dw_session")
	if err != nil {
		http.Error(w, "missing dw_session", http.StatusUnauthorized)
		return
	}
	user, ok := g.verifySession(cookie.Value, authority)
	if !ok {
		http.Error(w, "invalid dw_session", http.StatusUnauthorized)
		return
	}
	if user != inst.User {
		http.NotFound(w, r)
		return
	}
	switch inst.State {
	case model.StatePending, model.StateStarting, model.StateStopping:
		writeRetryPage(w, inst.State)
		return
	case model.StateRunning:
	default:
		http.Error(w, "instance is not running", http.StatusServiceUnavailable)
		return
	}

	dshCookie, cached, err := g.dshCookieFor(inst)
	if err != nil {
		http.Error(w, "dsh broker: "+err.Error(), http.StatusBadGateway)
		return
	}
	g.mgr.Touch(inst)

	fresh := !cached
	for attempt := 0; attempt < 2; attempt++ {
		refresh := false
		proxy := g.reverseProxy(inst, dshCookie, fresh, &refresh)
		proxy.ServeHTTP(w, r)
		if !refresh {
			return
		}
		// dsh rejected a cached cookie (it restarted or rotated its token).
		// Invalidate, exchange once, and retry exactly once. A second 401 is
		// turned into a 502 inside reverseProxy.
		g.invalidateDshCookie(inst)
		dshCookie, err = g.ensureDshCookie(inst, true)
		if err != nil {
			http.Error(w, "dsh broker: "+err.Error(), http.StatusBadGateway)
			return
		}
		fresh = true
	}
}

// reverseProxy builds the upstream proxy for one attempt. fresh tells the
// ModifyResponse hook whether the cookie was just exchanged: only a cached
// cookie may trigger a refresh, so a broken token cannot loop.
func (g *Gateway) reverseProxy(inst *model.Instance, cookie string, fresh bool, refresh *bool) *httputil.ReverseProxy {
	scheme := "http"
	if g.cfg != nil && g.cfg.Routing.PublicScheme != "" {
		scheme = g.cfg.Routing.PublicScheme
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", inst.Port)}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = inst.PublicHost
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Cookie", cookie)
			pr.Out.Header.Set("Origin", scheme+"://"+inst.PublicHost)
			pr.Out.Header.Set("X-Forwarded-Host", inst.PublicHost)
			pr.Out.Header.Set("X-Forwarded-Proto", scheme)
		},
		ModifyResponse: func(resp *http.Response) error {
			// Never forward dsh Set-Cookie to the browser: the upstream session
			// cookie is the gateway secret. TestProxyStripsUpstreamSetCookie
			// pins this.
			resp.Header.Del("Set-Cookie")
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "http://127.0.0.1:") {
				if u, err := url.Parse(loc); err == nil {
					u.Host = inst.PublicHost
					u.Scheme = scheme
					resp.Header.Set("Location", u.String())
				}
			}
			if resp.StatusCode == http.StatusUnauthorized {
				if fresh {
					return errFreshDshCookie
				}
				// Only a request without a body can be replayed. A cached 401 on
				// POST/PUT/PATCH/DELETE has already consumed its body, so it is
				// forwarded as-is instead of retried.
				if idempotentRequest(resp) {
					*refresh = true
					return errRefreshDshCookie
				}
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, perr error) {
			switch {
			case errors.Is(perr, errRefreshDshCookie):
				// Swallowed: serveResolved retries with a fresh cookie.
				return
			case errors.Is(perr, errFreshDshCookie):
				g.logf("proxy %s: dsh rejected a freshly exchanged cookie", inst.ID)
				http.Error(rw, "upstream error", http.StatusBadGateway)
			default:
				g.logf("proxy %s: %v", inst.ID, perr)
				http.Error(rw, "upstream error", http.StatusBadGateway)
			}
		},
		FlushInterval: 100 * time.Millisecond,
	}
}

func (g *Gateway) logf(format string, args ...any) {
	if g != nil && g.logger != nil {
		g.logger.Printf(format, args...)
	}
}

func writeRetryPage(w http.ResponseWriter, state model.State) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "3")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8">` +
		`<meta http-equiv="refresh" content="3">` +
		`<title>DeepWarper 启动中</title></head><body style="font-family:system-ui">` +
		`<h2>实例启动中</h2><p>正在等待本机进程就绪。本页每 3 秒自动刷新。</p>` +
		`<p>状态：` + html.EscapeString(string(state)) + `</p></body></html>`))
}

// dshCookieCacheVersion guards the broker cache format. Version 0 (the old
// {"cookie": "..."} file) is ignored and re-exchanged.
const dshCookieCacheVersion = 1

type dshCookieCache struct {
	Version     int       `json:"version"`
	InstanceID  string    `json:"instanceId"`
	Incarnation string    `json:"incarnation"`
	PublicHost  string    `json:"publicHost"`
	Cookie      string    `json:"cookie"`
	FetchedAt   time.Time `json:"fetchedAt"`
}

func (g *Gateway) dshCookiePath(inst *model.Instance) (string, error) {
	if inst == nil || !model.IsSafeFileComponent(inst.ID) {
		return "", errors.New("unsafe instance id")
	}
	if g == nil || g.cfg == nil {
		return "", errors.New("gateway is not configured")
	}
	return filepath.Join(g.cfg.Server.DataDir, "runtime", inst.ID, "dsh-session.json"), nil
}

// readDshCookie returns a cached cookie only when the record, incarnation and
// public host all match. Anything else is stale.
func (g *Gateway) readDshCookie(inst *model.Instance) (string, bool) {
	path, err := g.dshCookiePath(inst)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var c dshCookieCache
	if err := json.Unmarshal(data, &c); err != nil {
		return "", false
	}
	if c.Version != dshCookieCacheVersion || c.Cookie == "" {
		return "", false
	}
	if c.InstanceID != inst.ID || c.Incarnation != inst.Incarnation || c.PublicHost != inst.PublicHost {
		return "", false
	}
	return c.Cookie, true
}

func (g *Gateway) writeDshCookie(inst *model.Instance, cookie string) error {
	path, err := g.dshCookiePath(inst)
	if err != nil {
		return err
	}
	data, err := json.Marshal(dshCookieCache{
		Version:     dshCookieCacheVersion,
		InstanceID:  inst.ID,
		Incarnation: inst.Incarnation,
		PublicHost:  inst.PublicHost,
		Cookie:      cookie,
		FetchedAt:   time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Dir(path), path, data, 0o600)
}

func (g *Gateway) invalidateDshCookie(inst *model.Instance) {
	if path, err := g.dshCookiePath(inst); err == nil {
		_ = os.Remove(path)
	}
}

// dshCookieFor returns the cached cookie and whether it was cached, or
// exchanges a token when no valid cache exists.
func (g *Gateway) dshCookieFor(inst *model.Instance) (string, bool, error) {
	if cookie, ok := g.readDshCookie(inst); ok {
		return cookie, true, nil
	}
	cookie, err := g.ensureDshCookie(inst, false)
	return cookie, false, err
}

// dshLock serializes token exchanges per instance so concurrent first requests
// do not race each other into two dsh sessions.
func (g *Gateway) dshLock(id string) func() {
	g.dshMu.Lock()
	if g.dshLocks == nil {
		g.dshLocks = map[string]*sync.Mutex{}
	}
	m := g.dshLocks[id]
	if m == nil {
		m = &sync.Mutex{}
		g.dshLocks[id] = m
	}
	g.dshMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (g *Gateway) ensureDshCookie(inst *model.Instance, force bool) (string, error) {
	if inst == nil {
		return "", errors.New("instance is required")
	}
	unlock := g.dshLock(inst.ID)
	defer unlock()
	if !force {
		if cookie, ok := g.readDshCookie(inst); ok {
			return cookie, nil
		}
	}
	token, err := g.mgr.Token(inst)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	if token == "" {
		return "", errors.New("empty dsh token")
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
	if err := g.writeDshCookie(inst, cookie); err != nil {
		return "", err
	}
	return cookie, nil
}

// canonicalAuthority is the host:port string a session/ticket is bound to. It
// is built from the effective (allowlisted) host, so a session cookie minted
// for one subdomain never verifies on another. A default port (80/443) is
// normalized away so `host` and `host:443` are the same authority.
func canonicalAuthority(host string) string {
	host = normalizeHost(host)
	if host == "" {
		return ""
	}
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port == "80" || port == "443" {
			return h
		}
		return net.JoinHostPort(h, port)
	}
	return host
}

// idempotentRequest reports whether replaying the upstream request is safe.
// The method must be idempotent and the request must be bodyless: a GET, HEAD
// or OPTIONS carrying a body has already had that body consumed by the failed
// attempt, so retrying it would send an empty body upstream.
func idempotentRequest(resp *http.Response) bool {
	if resp == nil || resp.Request == nil {
		return true
	}
	req := resp.Request
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0 {
		return false
	}
	return true
}
