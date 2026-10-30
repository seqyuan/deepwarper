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
	if !p.ReleaseOwned(a, "a") {
		t.Fatal("owner must release its own port")
	}
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

func TestReserveRenewsSameOwner(t *testing.T) {
	p := New(24020, 24030)
	if err := p.Reserve(24025, "a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(24025, "a"); err != nil {
		t.Fatalf("same-owner renew: %v", err)
	}
	if owner, ok := p.Owner(24025); !ok || owner != "a" {
		t.Fatalf("owner: %q/%v", owner, ok)
	}
}

func TestReserveRejectsOutOfRange(t *testing.T) {
	p := New(24020, 24030)
	for _, port := range []int{0, -1, 24019, 24031, 70000} {
		if err := p.Reserve(port, "a"); err == nil {
			t.Fatalf("port %d must be rejected", port)
		}
	}
}

func TestReleaseOwnedIgnoresOtherOwner(t *testing.T) {
	p := New(24020, 24030)
	if err := p.Reserve(24025, "a"); err != nil {
		t.Fatal(err)
	}
	if p.ReleaseOwned(24025, "b") {
		t.Fatal("other owner must not release the reservation")
	}
	if owner, ok := p.Owner(24025); !ok || owner != "a" {
		t.Fatalf("reservation was lost: %q/%v", owner, ok)
	}
	if !p.ReleaseOwned(24025, "a") {
		t.Fatal("owner must release the reservation")
	}
	if _, ok := p.Owner(24025); ok {
		t.Fatal("reservation still present")
	}
	if p.ReleaseOwned(24025, "a") {
		t.Fatal("releasing an unowned port must report false")
	}
}

func TestReleaseOwnedAllowsReassignment(t *testing.T) {
	p := New(24020, 24030)
	if err := p.Reserve(24025, "a"); err != nil {
		t.Fatal(err)
	}
	if !p.ReleaseOwned(24025, "a") {
		t.Fatal("release failed")
	}
	if err := p.Reserve(24025, "b"); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if owner, _ := p.Owner(24025); owner != "b" {
		t.Fatalf("owner: %q", owner)
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
