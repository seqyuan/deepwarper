package gateway

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// loginAttempt is the per-key state of the login limiter. The window starts at
// the first failure; a lockout starts once max failures landed inside it.
type loginAttempt struct {
	failures     int
	first        time.Time
	last         time.Time
	blockedUntil time.Time
}

// loginLimiter is a small in-memory limiter keyed by client IP and by
// IP|username. It bounds online password guessing without an external store;
// a control-plane restart resets the counters, which is acceptable for a
// single-site deployment.
type loginLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	lockout time.Duration
	now     func() time.Time
	entries map[string]*loginAttempt
}

func newLoginLimiter(max int, window, lockout time.Duration) *loginLimiter {
	if max <= 0 {
		max = 10
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	if lockout <= 0 {
		lockout = 5 * time.Minute
	}
	return &loginLimiter{
		max:     max,
		window:  window,
		lockout: lockout,
		now:     time.Now,
		entries: map[string]*loginAttempt{},
	}
}

// allow reports whether key may attempt a login now. When it may not,
// retryAfter is the remaining lockout.
func (l *loginLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.entries[key]
	if a == nil {
		return true, 0
	}
	if now.Before(a.blockedUntil) {
		return false, a.blockedUntil.Sub(now)
	}
	if !a.blockedUntil.IsZero() || now.Sub(a.first) > l.window {
		// The lockout or the counting window is over: start clean.
		delete(l.entries, key)
	}
	return true, 0
}

// fail records one failed attempt. Reaching max inside the window blocks the
// key for the lockout duration.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.entries[key]
	if a == nil || (!a.blockedUntil.IsZero() && !now.Before(a.blockedUntil)) || now.Sub(a.first) > l.window {
		a = &loginAttempt{first: now}
		l.entries[key] = a
	}
	a.failures++
	a.last = now
	if a.failures >= l.max {
		a.blockedUntil = now.Add(l.lockout)
	}
}

// reset clears the counters after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// prune drops idle entries so the map cannot grow without bound.
func (l *loginLimiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, a := range l.entries {
		if now.Before(a.blockedUntil) {
			continue
		}
		if now.Sub(a.last) > l.window+l.lockout {
			delete(l.entries, key)
		}
	}
}

// loginLimit lazily builds the limiter for callers that construct a Gateway
// literal (tests, embedders). New sets it eagerly from config.
func (g *Gateway) loginLimit() *loginLimiter {
	g.limiterOnce.Do(func() {
		if g.limiter != nil {
			return
		}
		max, window, lockout := 10, 5*time.Minute, 5*time.Minute
		if g.cfg != nil {
			max, window, lockout = g.cfg.LoginLimit()
		}
		g.limiter = newLoginLimiter(max, window, lockout)
	})
	return g.limiter
}

// clientIP is the rate-limit key. X-Forwarded-For is only honored when the
// proxy headers are trusted, and the last hop is used: a proxy appends the
// address it saw, so the last entry is the one the proxy vouches for while
// earlier client-supplied entries are spoofable.
func (g *Gateway) clientIP(r *http.Request) string {
	if g != nil && g.cfg != nil && g.cfg.Routing.TrustProxyHeaders && r != nil {
		if v := headerLastValue(r.Header.Get("X-Forwarded-For")); v != "" {
			return hostOnly(normalizeHost(v))
		}
	}
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// writeRateLimited answers 429 with a Retry-After in whole seconds.
func writeRateLimited(w http.ResponseWriter, wait time.Duration) {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, "too many login attempts", http.StatusTooManyRequests)
}
