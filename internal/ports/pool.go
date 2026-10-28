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

func (p *Pool) Reserve(port int, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if other, ok := p.used[port]; ok && other != owner {
		return fmt.Errorf("port %d already reserved by %s", port, other)
	}
	p.used[port] = owner
	return nil
}

func (p *Pool) Release(port int) {
	p.mu.Lock()
	delete(p.used, port)
	p.mu.Unlock()
}
