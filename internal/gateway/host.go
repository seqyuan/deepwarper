package gateway

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// headerValue returns the first non-empty value of a possibly comma-separated
// forwarded header. Only a trusted proxy may supply these headers.
func headerValue(raw string) string {
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			return v
		}
	}
	return ""
}

// headerLastValue is the mirror image for X-Forwarded-For, where the nearest
// (trusted) proxy appends the last hop.
func headerLastValue(raw string) string {
	parts := strings.Split(raw, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(parts[i]); v != "" {
			return v
		}
	}
	return ""
}

// normalizeHost lowercases the host, drops a trailing dot and keeps an
// optional port. Rejection is the allowlist's job, not this helper's.
func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return strings.TrimSuffix(strings.ToLower(raw), ".")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// hostOnly strips a port from an already normalized host.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// effectiveHost is the host the browser used. X-Forwarded-Host is only
// honored when routing.trustProxyHeaders is set, otherwise any client could
// pick its own authority.
func (g *Gateway) effectiveHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	raw := r.Host
	if g != nil && g.cfg != nil && g.cfg.Routing.TrustProxyHeaders {
		if fwd := headerValue(r.Header.Get("X-Forwarded-Host")); fwd != "" {
			raw = fwd
		}
	}
	return normalizeHost(raw)
}

// hostSuffix is the configured apex, normalized the same way as hosts.
func (g *Gateway) hostSuffix() string {
	if g == nil || g.cfg == nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(g.cfg.Routing.HostSuffix)), ".")
}

// hostAllowed implements the routing allowlist: the apex itself or exactly one
// label below it. Multi-label, unrelated hosts and anything carrying
// userinfo/path/whitespace are rejected (DNS rebinding, login CSRF).
func (g *Gateway) hostAllowed(host string) bool {
	suffix := g.hostSuffix()
	if suffix == "" || host == "" {
		return false
	}
	if strings.ContainsAny(host, " \t\r\n\x00/@\\?#") {
		return false
	}
	name := hostOnly(host)
	if name == "" || strings.Contains(name, "@") || strings.Contains(name, "..") {
		return false
	}
	if port := explicitPort(host); port != "" && !isPort(port) {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	if name == suffix {
		return true
	}
	if !strings.HasSuffix(name, "."+suffix) {
		return false
	}
	label := strings.TrimSuffix(name, "."+suffix)
	return label != "" && !strings.Contains(label, ".")
}

// isApex reports whether host is the bare routing suffix (no instance label).
func (g *Gateway) isApex(host string) bool {
	suffix := g.hostSuffix()
	return suffix != "" && hostOnly(host) == suffix
}

// requiresHostCheck lists the routes that must sit behind the allowlist.
// /healthz stays reachable for probes; /api/* is Bearer-authenticated and is
// exempt when the request actually carries a Bearer credential.
func requiresHostCheck(r *http.Request) bool {
	if r.URL.Path == "/healthz" {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return false
		}
	}
	return true
}

// requestScheme is the browser-facing scheme: a trusted X-Forwarded-Proto wins,
// otherwise routing.publicScheme.
func (g *Gateway) requestScheme(r *http.Request) string {
	if g != nil && g.cfg != nil {
		if g.cfg.Routing.TrustProxyHeaders && r != nil {
			if proto := strings.ToLower(headerValue(r.Header.Get("X-Forwarded-Proto"))); proto == "http" || proto == "https" {
				return proto
			}
		}
		if g.cfg.Routing.PublicScheme != "" {
			return strings.ToLower(g.cfg.Routing.PublicScheme)
		}
	}
	return "http"
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// normalizePort drops the default port for the scheme so "https://host" and
// "https://host:443" compare equal.
func normalizePort(port, scheme string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return ""
	}
	switch strings.ToLower(scheme) {
	case "http":
		if port == "80" {
			return ""
		}
	case "https":
		if port == "443" {
			return ""
		}
	}
	return port
}

// expectedPort is the port the browser must have used: an explicit host port,
// then a trusted X-Forwarded-Port, then the configured public port.
func (g *Gateway) expectedPort(r *http.Request) (scheme, port string) {
	scheme = g.requestScheme(r)
	if r != nil {
		if explicit := explicitPort(g.effectiveHost(r)); explicit != "" {
			return scheme, normalizePort(explicit, scheme)
		}
		if g.cfg != nil && g.cfg.Routing.TrustProxyHeaders {
			if forwarded := headerValue(r.Header.Get("X-Forwarded-Port")); isPort(forwarded) {
				return scheme, normalizePort(forwarded, scheme)
			}
		}
	}
	if g != nil && g.cfg != nil {
		if p := g.cfg.EffectivePublicPort(); p > 0 {
			return scheme, normalizePort(strconv.Itoa(p), scheme)
		}
	}
	return scheme, ""
}

func explicitPort(host string) string {
	if _, port, err := net.SplitHostPort(host); err == nil {
		return port
	}
	return ""
}

func isPort(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535
}

// originAllowed validates the Origin header of a state-changing request. An
// empty Origin is accepted only for same-origin/non-cross-site fetches;
// otherwise scheme, host and port must match what the browser used.
func (g *Gateway) originAllowed(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return !strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return false
	}
	scheme, port := g.expectedPort(r)
	if !strings.EqualFold(u.Scheme, scheme) {
		return false
	}
	originHost := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	requestHost := hostOnly(g.effectiveHost(r))
	if originHost == "" || requestHost == "" || originHost != requestHost {
		return false
	}
	originPort := u.Port()
	if originPort == "" {
		originPort = defaultPort(u.Scheme)
	}
	return normalizePort(originPort, scheme) == port
}
