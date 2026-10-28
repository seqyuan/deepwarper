package sge

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TunnelRuntime is a started data channel (an `ssh -L` session in production).
type TunnelRuntime interface {
	Alive() bool
	Stop(ctx context.Context) error
	PID() int
}

// TunnelFactory starts the data channel for t. It exists so the backend can be
// tested without a cluster (and without spawning ssh).
type TunnelFactory func(ctx context.Context, t *Tunnel) (TunnelRuntime, error)

// defaultTunnelFactory runs the real `ssh -L`.
func defaultTunnelFactory(ctx context.Context, t *Tunnel) (TunnelRuntime, error) {
	if err := t.Start(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// Tunnel is an `ssh -L` local forward from the login node to a process on a
// compute node.
//
// Direction matters and is the reason this is -L and not -R: the trust direction
// that exists on HPC sites is login -> compute (that is how a user reaches the
// node running their job), whereas compute -> login SSH is usually blocked. A
// local forward therefore needs no cluster configuration change.
//
// The tunnel is what makes "instances listen on loopback only" survive contact
// with a scheduler: dsh binds 127.0.0.1 on the compute node, ssh carries that to
// a loopback port on the login node, and the gateway proxies to it exactly as it
// would to a local service.
type Tunnel struct {
	// LocalPort is the loopback port on the login node.
	LocalPort int
	// Node is the compute node's hostname.
	Node string
	// RemoteHost/RemotePort is the target on the compute node (loopback there).
	RemoteHost string
	RemotePort int
	// User is the ssh login; empty means the current user.
	User string
	// SSHBin overrides ssh.
	SSHBin string
	// ExtraArgs are site options (-i key, -o UserKnownHostsFile=..., -J jump).
	ExtraArgs []string
	// ReadyTimeout bounds how long to wait for the forward to actually work.
	ReadyTimeout time.Duration
	// Dial is a test seam; nil means a real TCP dial to 127.0.0.1:LocalPort.
	Dial DialFunc

	once sync.Once
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (t *Tunnel) sshBin() string {
	if t.SSHBin != "" {
		return t.SSHBin
	}
	return "ssh"
}

// hostRE is deliberately narrower than a general URI host: it is a compute-node
// DNS name or an IP literal. In particular it must not start with '-', because
// the value is handed to ssh as its destination argument and ssh would parse
// "-oProxyCommand=..." as an option and execute it on the login node.
var hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// ValidHost reports whether a host string is safe to hand to ssh as a
// destination. The value originates in a file written on a compute node, so it
// is not trusted further than "looks like a hostname or an IP literal".
func ValidHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, "-") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true // IPv4/IPv6 literals are valid ssh destinations
	}
	return hostRE.MatchString(host)
}

// Args renders the ssh command line.
//
// Note what is *not* here: no GatewayPorts, no remote forward, no shell. The
// forward is loopback-to-loopback and the session runs no command, which is the
// smallest surface that can carry the traffic.
func (t *Tunnel) Args() ([]string, error) {
	if !ValidHost(t.Node) {
		return nil, fmt.Errorf("refusing to tunnel to %q: not a valid host name", t.Node)
	}
	if t.LocalPort <= 0 || t.LocalPort > 65535 {
		return nil, fmt.Errorf("refusing to tunnel: local port %d is not usable", t.LocalPort)
	}
	if t.RemotePort <= 0 || t.RemotePort > 65535 {
		return nil, fmt.Errorf("refusing to tunnel: remote port %d is not usable", t.RemotePort)
	}
	remoteHost := t.RemoteHost
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	if !ValidHost(remoteHost) {
		return nil, fmt.Errorf("refusing to tunnel: remote host %q is not valid", remoteHost)
	}
	dest := t.Node
	if t.User != "" {
		dest = t.User + "@" + t.Node
	}
	args := []string{
		"-N", // no command: this session exists only to carry the forward
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes", // fail fast instead of silently not forwarding
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ClearAllForwardings=no",
		"-L", fmt.Sprintf("127.0.0.1:%d:%s:%d", t.LocalPort, remoteHost, t.RemotePort),
	}
	args = append(args, t.ExtraArgs...)
	// `--` ends option parsing: even if a caller slipped a value through, ssh
	// must treat it as a destination, never as an option. ValidHost is the first
	// lock on that door; this is the second.
	return append(args, "--", dest), nil
}

// Start launches the tunnel and waits until the forward actually works.
//
// Waiting is not optional: ssh reports success for the session before the
// channel is usable, and publishing a route to a not-yet-forwarding port is
// exactly the race that produces intermittent 502s.
func (t *Tunnel) Start(ctx context.Context) error {
	t.once.Do(func() {
		args, err := t.Args()
		if err != nil {
			t.err = err
			return
		}
		cmd := exec.Command(t.sshBin(), args...)
		// Its own process group so Stop can take down ssh and anything it spawned
		// without touching the caller's group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.err = fmt.Errorf("ssh -L: %w", err)
			return
		}
		t.cmd = cmd
		t.done = make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(t.done)
		}()

		timeout := t.ReadyTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		exited := func() bool {
			select {
			case <-t.done:
				return true
			default:
				return false
			}
		}
		if err := waitDialable(ctx, t.LocalPort, timeout, t.dial, exited); err != nil {
			_ = t.stop(context.Background())
			t.err = err
		}
	})
	return t.err
}

func (t *Tunnel) dial(host string, port int, timeout time.Duration) bool {
	if t.Dial != nil {
		return t.Dial(host, port, timeout)
	}
	return dialTCP(port, timeout)
}

// waitDialable polls the loopback port until it accepts a connection, the
// context is done, the budget expires, or the ssh process exits.
func waitDialable(ctx context.Context, port int, timeout time.Duration, dial func(string, int, time.Duration) bool, exited func() bool) error {
	if dial == nil {
		dial = func(_ string, p int, d time.Duration) bool { return dialTCP(p, d) }
	}
	deadline := time.Now().Add(timeout)
	for {
		if dial("127.0.0.1", port, time.Second) {
			return nil
		}
		if exited != nil && exited() {
			return fmt.Errorf("ssh exited before the forward to 127.0.0.1:%d became usable", port)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("forward to 127.0.0.1:%d was not usable within %s", port, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Alive reports whether the tunnel is still carrying traffic.
func (t *Tunnel) Alive() bool {
	if t.cmd == nil || t.cmd.Process == nil {
		return false
	}
	if t.done != nil {
		select {
		case <-t.done:
			return false
		default:
		}
	}
	return t.dial("127.0.0.1", t.LocalPort, time.Second)
}

// Stop tears the tunnel down. Idempotent, because Stop can race the reaper.
func (t *Tunnel) Stop(ctx context.Context) error {
	t.once.Do(func() {})
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	return t.stop(ctx)
}

func (t *Tunnel) stop(ctx context.Context) error {
	proc := t.cmd.Process
	if proc == nil {
		return nil
	}
	// Signal the group: ssh may have children (a control master, a helper).
	signalGroup := func(sig syscall.Signal) {
		if pgid, err := syscall.Getpgid(proc.Pid); err == nil && pgid > 0 {
			_ = syscall.Kill(-pgid, sig)
			return
		}
		_ = proc.Signal(sig)
	}
	signalGroup(syscall.SIGTERM)

	if t.done != nil {
		select {
		case <-t.done:
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	signalGroup(syscall.SIGKILL)
	if t.done != nil {
		select {
		case <-t.done:
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("ssh tunnel to %s did not exit", t.Node)
		}
	}
	return nil
}

// PID is exposed for the stale-tunnel bookkeeping and for tests.
func (t *Tunnel) PID() int {
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

// dialTCP is the production "is this loopback port usable" check.
func dialTCP(port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
