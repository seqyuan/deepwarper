// Command dw-local is a deliberately small, single-machine proof of concept
// for DeepWarper. It validates the parts that are hard to change later:
//
//   - one dsh web process per user, with a separate DSH_HOME and workspace
//   - a subdomain gateway (alice.localhost -> alice's instance)
//   - a one-time platform launch ticket (platform auth stays outside dsh)
//   - dsh's launch-token broker: the browser never sees dsh's ?token=
//   - persistence: stopping an instance keeps its home and profile
//
// It is not the production control plane. It has no resource limits, and the
// ticket/session stores are in memory.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	Listen        string
	DataDir       string
	DSHBin        string
	Domain        string
	PlatformToken string
	PublicPort    string
}

type instance struct {
	mu sync.RWMutex

	User       string
	Slug       string
	Home       string
	Workspace  string
	PublicHost string
	Port       int
	Token      string
	Cookie     string
	State      string
	Err        string
	LastActive time.Time

	cmd  *exec.Cmd
	done chan struct{}
}

type ticket struct {
	User string
	Exp  time.Time
}

type session struct {
	User string
	Exp  time.Time
}

type manager struct {
	cfg config
	log *log.Logger

	startMu sync.Mutex

	mu       sync.Mutex
	byUser   map[string]*instance
	bySlug   map[string]*instance
	tickets  map[string]ticket
	sessions map[string]session

	httpClient *http.Client
}

func newManager(cfg config, logger *log.Logger) *manager {
	return &manager{
		cfg:      cfg,
		log:      logger,
		byUser:   map[string]*instance{},
		bySlug:   map[string]*instance{},
		tickets:  map[string]ticket{},
		sessions: map[string]session{},
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (m *manager) launch(user, display string) (*instance, error) {
	if strings.TrimSpace(user) == "" {
		return nil, errors.New("user_id is required")
	}

	m.startMu.Lock()
	defer m.startMu.Unlock()

	m.mu.Lock()
	if inst := m.byUser[user]; inst != nil {
		inst.mu.RLock()
		state := inst.State
		inst.mu.RUnlock()
		if state == "running" {
			m.mu.Unlock()
			return inst, nil
		}
	}
	m.mu.Unlock()

	slug := m.uniqueSlug(user)
	home := filepath.Join(m.cfg.DataDir, "homes", slug)
	workspace := filepath.Join(m.cfg.DataDir, "workspaces", slug)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create home: %w", err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	publicHost := fmt.Sprintf("%s.%s:%s", slug, m.cfg.Domain, m.cfg.PublicPort)
	inst := &instance{
		User:       user,
		Slug:       slug,
		Home:       home,
		Workspace:  workspace,
		PublicHost: publicHost,
		State:      "starting",
		done:       make(chan struct{}),
	}

	m.mu.Lock()
	m.byUser[user] = inst
	m.bySlug[slug] = inst
	m.mu.Unlock()

	if err := m.start(inst); err != nil {
		inst.mu.Lock()
		inst.State = "failed"
		inst.Err = err.Error()
		inst.mu.Unlock()
		return nil, err
	}
	return inst, nil
}

func (m *manager) start(inst *instance) error {
	// dsh prints the *public* URL when --public-url is set, so the listener
	// port cannot be read from that line. Allocate it here and tell dsh.
	port, err := freePort()
	if err != nil {
		return err
	}
	args := []string{
		"web",
		"--no-open",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--public-url", "http://" + inst.PublicHost + "/",
		"--trusted-host", inst.Slug + "." + m.cfg.Domain,
	}
	cmd := exec.Command(m.cfg.DSHBin, args...)
	cmd.Dir = inst.Workspace
	cmd.Env = append(os.Environ(),
		"DSH_HOME="+inst.Home,
		"HOME="+inst.Home,
	)
	// Own process group so Stop can take dsh's children with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start dsh: %w", err)
	}

	inst.mu.Lock()
	inst.cmd = cmd
	inst.mu.Unlock()

	ready := make(chan string, 1)
	exit := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		exit <- err
	}()
	go scanDSH(stdout, ready)
	go scanStderr(stderr, m.log, inst.Slug)

	select {
	case raw := <-ready:
		token, err := parseReadyToken(raw)
		if err != nil {
			m.stopProcess(cmd)
			return err
		}
		inst.mu.Lock()
		inst.Port = port
		inst.Token = token
		inst.State = "running"
		inst.LastActive = time.Now()
		inst.mu.Unlock()

		if err := m.exchangeCookie(inst); err != nil {
			m.stopProcess(cmd)
			return fmt.Errorf("dsh token exchange: %w", err)
		}

		go func() {
			err := <-exit
			if err != nil {
				m.log.Printf("instance %s exited: %v", inst.Slug, err)
			} else {
				m.log.Printf("instance %s exited", inst.Slug)
			}
			inst.mu.Lock()
			if inst.State != "stopping" {
				inst.State = "stopped"
			}
			inst.cmd = nil
			inst.mu.Unlock()
			close(inst.done)
		}()
		m.log.Printf("instance %s ready on 127.0.0.1:%d (home=%s)", inst.Slug, port, inst.Home)
		return nil

	case err := <-exit:
		if err == nil {
			err = errors.New("dsh exited before printing its URL")
		}
		return fmt.Errorf("dsh exited before ready: %w", err)

	case <-time.After(90 * time.Second):
		m.stopProcess(cmd)
		return errors.New("timed out waiting for dsh to print its URL")
	}
}

// scanDSH waits for the one line that carries the listener and launch token.
// It deliberately does not log the line: it contains a process credential.
func scanDSH(r interface{ Read([]byte) (int, error) }, ready chan<- string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "dsh web:") {
			select {
			case ready <- line:
			default:
			}
		}
	}
}

func scanStderr(r interface{ Read([]byte) (int, error) }, logger *log.Logger, slug string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Keep stderr visible for diagnostics, but never echo a token URL.
		if strings.Contains(line, "token=") {
			logger.Printf("[%s] %s", slug, "dsh startup line suppressed (contains token)")
			continue
		}
		logger.Printf("[%s] %s", slug, line)
	}
}

var readyRE = regexp.MustCompile(`^dsh web:\s+(\S+)`)

func parseReadyToken(line string) (string, error) {
	m := readyRE.FindStringSubmatch(line)
	if len(m) != 2 {
		return "", fmt.Errorf("cannot parse dsh ready line")
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return "", fmt.Errorf("parse dsh URL: %w", err)
	}
	token := u.Query().Get("token")
	if token == "" {
		return "", errors.New("dsh URL has no token")
	}
	return token, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func (m *manager) exchangeCookie(inst *instance) error {
	inst.mu.RLock()
	port, token, publicHost := inst.Port, inst.Token, inst.PublicHost
	inst.mu.RUnlock()
	if port == 0 || token == "" {
		return errors.New("instance is not ready")
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", port, url.QueryEscape(token))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Host = publicHost
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var dshCookie string
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, "dsh-auth-") {
			dshCookie = c.Name + "=" + c.Value
			break
		}
	}
	if dshCookie == "" {
		return fmt.Errorf("token exchange returned %d without dsh-auth cookie", resp.StatusCode)
	}
	inst.mu.Lock()
	inst.Cookie = dshCookie
	inst.mu.Unlock()
	return nil
}

func (m *manager) stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func (m *manager) stop(user string) error {
	m.mu.Lock()
	inst := m.byUser[user]
	m.mu.Unlock()
	if inst == nil {
		return errors.New("unknown user")
	}
	inst.mu.Lock()
	cmd, done := inst.cmd, inst.done
	if cmd == nil {
		inst.State = "stopped"
		inst.mu.Unlock()
		return nil
	}
	inst.State = "stopping"
	inst.mu.Unlock()

	m.stopProcess(cmd)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		return errors.New("timed out stopping dsh")
	}
	inst.mu.Lock()
	if inst.State != "stopped" {
		inst.State = "stopped"
	}
	inst.Cookie = ""
	inst.Token = ""
	inst.mu.Unlock()
	return nil
}

func (m *manager) stopAll() {
	m.mu.Lock()
	users := make([]string, 0, len(m.byUser))
	for user := range m.byUser {
		users = append(users, user)
	}
	m.mu.Unlock()
	for _, user := range users {
		_ = m.stop(user)
	}
}

func (m *manager) uniqueSlug(user string) string {
	base := slugify(user)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.bySlug[base]; existing == nil || existing.User == user {
		return base
	}
	sum := sha256.Sum256([]byte(user))
	return base + "-" + hex.EncodeToString(sum[:3])
}

var slugRE = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
	}
	if s == "" {
		s = "user"
	}
	return s
}

func (m *manager) instanceForHost(host string) *instance {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	suffix := "." + strings.ToLower(m.cfg.Domain)
	if !strings.HasSuffix(hostname, suffix) {
		return nil
	}
	slug := strings.TrimSuffix(hostname, suffix)
	if slug == "" || strings.Contains(slug, ".") {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bySlug[slug]
}

func (m *manager) newTicket(user string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	t := hex.EncodeToString(b)
	m.mu.Lock()
	m.tickets[t] = ticket{User: user, Exp: time.Now().Add(60 * time.Second)}
	m.mu.Unlock()
	return t
}

func (m *manager) consumeTicket(t string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tk, ok := m.tickets[t]
	if !ok {
		return "", false
	}
	delete(m.tickets, t)
	if time.Now().After(tk.Exp) {
		return "", false
	}
	return tk.User, true
}

func (m *manager) newSession(user string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := hex.EncodeToString(b)
	m.mu.Lock()
	m.sessions[s] = session{User: user, Exp: time.Now().Add(12 * time.Hour)}
	m.mu.Unlock()
	return s
}

func (m *manager) sessionUser(s string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[s]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.Exp) {
		delete(m.sessions, s)
		return "", false
	}
	return sess.User, true
}

func (m *manager) handlePlatformLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	inst, err := m.launch(body.UserID, body.DisplayName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	t := m.newTicket(body.UserID)
	scheme := "http"
	url := fmt.Sprintf("%s://%s/_dw/launch?ticket=%s", scheme, inst.PublicHost, t)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": url})
}

func (m *manager) handleLaunchTicket(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("ticket")
	user, ok := m.consumeTicket(t)
	if !ok {
		http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
		return
	}
	inst := m.instanceForHost(r.Host)
	if inst == nil || inst.User != user {
		http.Error(w, "ticket does not belong to this host", http.StatusForbidden)
		return
	}
	s := m.newSession(user)
	http.SetCookie(w, &http.Cookie{
		Name:     "dw_session",
		Value:    s,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 60 * 60,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (m *manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	instances := make([]map[string]any, 0, len(m.byUser))
	for _, inst := range m.byUser {
		inst.mu.RLock()
		instances = append(instances, map[string]any{
			"user":       inst.User,
			"slug":       inst.Slug,
			"state":      inst.State,
			"port":       inst.Port,
			"home":       inst.Home,
			"workspace":  inst.Workspace,
			"public_url": "http://" + inst.PublicHost + "/",
			"cookie_set": inst.Cookie != "",
			"error":      inst.Err,
		})
		inst.mu.RUnlock()
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(instances)
}

func (m *manager) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := m.stop(body.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *manager) authorized(r *http.Request) bool {
	return m.cfg.PlatformToken != "" &&
		r.Header.Get("Authorization") == "Bearer "+m.cfg.PlatformToken
}

func (m *manager) handleProxy(w http.ResponseWriter, r *http.Request) {
	inst := m.instanceForHost(r.Host)
	if inst == nil {
		http.NotFound(w, r)
		return
	}
	cookie, err := r.Cookie("dw_session")
	if err != nil {
		http.Error(w, "missing dw session", http.StatusUnauthorized)
		return
	}
	user, ok := m.sessionUser(cookie.Value)
	if !ok || user != inst.User {
		http.Error(w, "session does not belong to this instance", http.StatusUnauthorized)
		return
	}

	inst.mu.RLock()
	state, port, dshCookie, publicHost := inst.State, inst.Port, inst.Cookie, inst.PublicHost
	inst.mu.RUnlock()
	if state != "running" || port == 0 {
		http.Error(w, "instance is not running", http.StatusServiceUnavailable)
		return
	}
	if dshCookie == "" {
		if err := m.exchangeCookie(inst); err != nil {
			http.Error(w, "dsh token broker failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		inst.mu.RLock()
		dshCookie = inst.Cookie
		inst.mu.RUnlock()
	}
	inst.mu.Lock()
	inst.LastActive = time.Now()
	inst.mu.Unlock()

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = publicHost
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Cookie", dshCookie)
			// The browser sees http://<slug>.<domain>; dsh's Host/Origin fence
			// compares authority, so make Origin match the Host we forward.
			pr.Out.Header.Set("Origin", "http://"+publicHost)
			pr.Out.Header.Set("X-Forwarded-Host", publicHost)
			pr.Out.Header.Set("X-Forwarded-Proto", "http")
			pr.Out.Header.Set("X-Forwarded-For", clientIP(pr.In))
		},
		ModifyResponse: func(resp *http.Response) error {
			// dsh cookies are gateway-owned; the browser only keeps dw_session.
			resp.Header.Del("Set-Cookie")
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "http://127.0.0.1:") {
				if u, err := url.Parse(loc); err == nil {
					u.Host = publicHost
					resp.Header.Set("Location", u.String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			m.log.Printf("proxy error for %s: %v", inst.Slug, err)
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
		FlushInterval: 100 * time.Millisecond,
	}
	proxy.ServeHTTP(w, r)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func main() {
	listen := flag.String("listen", "127.0.0.1:7658", "gateway listen address")
	dataDir := flag.String("data", "./poc-data", "state, homes and workspaces")
	dshBin := flag.String("dsh", "dsh", "path to the dsh executable")
	domain := flag.String("domain", "localhost", "subdomain suffix, e.g. localhost")
	platformToken := flag.String("platform-token", "poc-token", "bearer token the platform uses")
	flag.Parse()

	_, port, err := net.SplitHostPort(*listen)
	if err != nil || port == "" {
		log.Fatalf("-listen must be host:port: %v", err)
	}
	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	if err := os.MkdirAll(absData, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	logger := log.New(os.Stdout, "[dw-local] ", log.LstdFlags)
	cfg := config{
		Listen:        *listen,
		DataDir:       absData,
		DSHBin:        *dshBin,
		Domain:        *domain,
		PlatformToken: *platformToken,
		PublicPort:    port,
	}
	m := newManager(cfg, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/platform/launch", m.handlePlatformLaunch)
	mux.HandleFunc("/api/local/status", m.handleStatus)
	mux.HandleFunc("/api/local/stop", m.handleStop)
	mux.HandleFunc("/_dw/launch", m.handleLaunchTicket)
	mux.HandleFunc("/", m.handleProxy)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Printf("listening on http://%s (subdomains of %s)", cfg.Listen, cfg.Domain)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	logger.Printf("shutting down")
	m.stopAll()
	_ = srv.Close()
}
