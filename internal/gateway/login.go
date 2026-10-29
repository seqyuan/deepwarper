package gateway

import (
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/plugin"
)

func (g *Gateway) loginEnabled() bool {
	return g != nil && g.cfg != nil && strings.TrimSpace(g.cfg.Auth.UsersDir) != ""
}

func (g *Gateway) currentUser(r *http.Request) (string, bool) {
	c, err := r.Cookie("dw_session")
	if err != nil || c.Value == "" {
		return "", false
	}
	return g.verifySession(c.Value, canonicalAuthority(r.Host))
}

func (g *Gateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !g.loginEnabled() {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html><head><meta charset="utf-8"><title>登录</title></head>
<body>
<form method="post" action="/login">
<label>用户名 <input name="username" autocomplete="username"></label>
<label>密码 <input name="password" type="password" autocomplete="current-password"></label>
<button type="submit">登录</button>
</form>
</body></html>`))
	case http.MethodPost:
		if !originAllowed(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.PostFormValue("username"))
		password := r.PostFormValue("password")
		ok, err := auth.Authenticate(g.cfg.Auth.UsersDir, name, password)
		if err != nil {
			http.Error(w, "login error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		value, err := g.signSession(name, canonicalAuthority(r.Host))
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
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func originAllowed(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	oHost, oPort := splitHost(u.Host)
	if oPort == "" {
		if u.Scheme == "https" {
			oPort = "443"
		} else {
			oPort = "80"
		}
	}
	rHost, rPort := splitHost(r.Host)
	if rPort == "" {
		rPort = "80"
	}
	return strings.EqualFold(oHost, rHost) && oPort == rPort
}

func splitHost(hostport string) (string, string) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return h, p
}

func (g *Gateway) handleUserHome(w http.ResponseWriter, r *http.Request) {
	user, ok := g.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	inst, err := g.mgr.ByUser(user)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if inst == nil || inst.State != model.StateRunning {
		writeNotRunning(w)
		return
	}
	g.serveResolved(w, r, inst)
}

func writeNotRunning(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html>
<html><head><meta charset="utf-8"><title>实例未运行</title></head>
<body><h1>实例未运行</h1><p>该用户的实例尚未运行，请由管理员启动后再访问。</p></body></html>`))
}

func (g *Gateway) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.loginEnabled() {
		http.NotFound(w, r)
		return
	}
	user, ok := g.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	rec, err := auth.Load(g.cfg.Auth.UsersDir, user)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	entries, err := plugin.LoadCatalog(filepath.Join(filepath.Dir(g.cfg.Auth.UsersDir), "catalog.yaml"))
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	enabled := map[string]bool{}
	for _, name := range rec.Plugins {
		enabled[name] = true
	}
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>插件</title></head><body><h1>插件</h1><ul>")
	if len(entries) == 0 {
		b.WriteString("<li>无插件</li>")
	}
	for _, e := range entries {
		state := "未启用"
		if enabled[e.Name] {
			state = "已启用"
		}
		b.WriteString("<li>")
		b.WriteString(html.EscapeString(e.Name))
		b.WriteString(": ")
		b.WriteString(state)
		b.WriteString("</li>")
	}
	b.WriteString("</ul></body></html>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (g *Gateway) handleMounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.loginEnabled() {
		http.NotFound(w, r)
		return
	}
	user, ok := g.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	rec, err := auth.Load(g.cfg.Auth.UsersDir, user)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>挂载</title></head><body><h1>挂载</h1><ul>")
	if len(rec.Mounts) == 0 {
		b.WriteString("<li>无挂载</li>")
	}
	for _, m := range rec.Mounts {
		b.WriteString("<li>")
		b.WriteString(html.EscapeString(m.Name))
		b.WriteString(" ")
		b.WriteString(html.EscapeString(m.Kind))
		b.WriteString(" ")
		b.WriteString(html.EscapeString(m.Target))
		b.WriteString(" ")
		b.WriteString(html.EscapeString(m.Mode))
		b.WriteString("</li>")
	}
	b.WriteString("</ul></body></html>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
