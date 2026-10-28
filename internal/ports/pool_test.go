package ports

import (
	"net"
	"testing"
)

func TestAcquireReturnsBindableUniquePorts(t *testing.T) {
	p := New(24000, 24010)
	a, err := p.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Acquire("b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("same port allocated twice: %d", a)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+itoa(a))
	if err != nil {
		t.Fatalf("allocated port %d is not bindable: %v", a, err)
	}
	l.Close()
	p.Release(a)
	c, err := p.Acquire("c")
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("released port not reused: got %d want %d", c, a)
	}
}

func TestReserveRejectsOtherOwner(t *testing.T) {
	p := New(24020, 24030)
	if err := p.Reserve(24025, "a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(24025, "b"); err == nil {
		t.Fatal("expected conflict")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
