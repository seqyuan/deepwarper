package ports

import (
	"fmt"
	"net"
	"sync"
)

type Pool struct {
	mu   sync.Mutex
	lo   int
	hi   int
	used map[int]string
}

func New(lo, hi int) *Pool {
	return &Pool{lo: lo, hi: hi, used: map[int]string{}}
}

func (p *Pool) Acquire(owner string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.lo; port <= p.hi; port++ {
		if _, ok := p.used[port]; ok {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = l.Close()
		p.used[port] = owner
		return port, nil
	}
	return 0, fmt.Errorf("no free port in range %d-%d", p.lo, p.hi)
}

// Reserve claims port for owner, or renews an existing reservation held by
// the same owner. Ports outside the pool range are rejected.
func (p *Pool) Reserve(port int, owner string) error {
	if port <= 0 || port < p.lo || port > p.hi {
		return fmt.Errorf("port %d is outside the pool range %d-%d", port, p.lo, p.hi)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if other, ok := p.used[port]; ok && other != owner {
		return fmt.Errorf("port %d already reserved by %s", port, other)
	}
	p.used[port] = owner
	return nil
}

// ReleaseOwned drops the reservation only when owner still holds it. It
// reports whether the reservation was removed.
func (p *Pool) ReleaseOwned(port int, owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.used[port]
	if !ok || cur != owner {
		return false
	}
	delete(p.used, port)
	return true
}

// Owner returns the current reservation holder for port.
func (p *Pool) Owner(port int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	owner, ok := p.used[port]
	return owner, ok
}
