package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/distribution"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/ports"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
	"github.com/seqyuan/deepwarper/internal/runtime/local"
	"github.com/seqyuan/deepwarper/internal/runtime/sge"
	"github.com/seqyuan/deepwarper/internal/state"
)

type Request struct {
	User         string
	DisplayName  string
	Distribution string
	Workspace    string
	Force        bool
}

type Manager struct {
	cfg   *config.Config
	store *state.Store
	ports *ports.Pool
	// backends is every runtime this site configured. New instances go to
	// active (cfg.Runtime.Default); adopted ones stay on the backend that owns
	// them, so a config change cannot orphan a running instance.
	backends map[string]runtime.Backend
	active   runtime.Backend
	guard    *state.MemGuard
	mu       sync.Mutex
	last     map[string]time.Time
	handles  map[string]runtime.Handle
	logf     func(format string, args ...any)
}

// LocalOptions is the config -> local backend mapping. It is exported so
// `dw doctor` can print the effective limiter without creating a store.
func LocalOptions(cfg *config.Config) local.Options {
	return local.Options{
		DwBin:             cfg.Runtime.DWBin,
		Limiter:           cfg.Runtime.Local.Limiter,
		SystemdUser:       cfg.Runtime.Local.SystemdUser.String(),
		ReadyWait:         cfg.ReadyTimeout(),
		TasksMax:          cfg.Runtime.Local.TasksMax,
		StartTimeout:      cfg.SystemdStartTimeout(),
		StopGrace:         cfg.StopGrace(),
		UnitStdio:         cfg.Runtime.Local.UnitStdio,
		PrlimitCPUSeconds: cfg.Runtime.Local.PrlimitCPUSeconds,
	}
}

func New(cfg *config.Config) (*Manager, error) {
	if cfg.Runtime.DWBin == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.Runtime.DWBin = exe
		}
	}
	pool := newPortPool(cfg)
	backends := map[string]runtime.Backend{
		"local": local.New(LocalOptions(cfg)),
	}
	if cfg.Runtime.SGE.IsEnabled() {
		backend, err := sge.FromConfig(cfg)
		if err != nil {
			// A configured-but-broken backend is a config bug, not something to
			// paper over: a silent fallback would run heavy work on a login node.
			return nil, err
		}
		backend.SetPorts(pool)
		backends["sge"] = backend
	}
	want := strings.ToLower(strings.TrimSpace(cfg.Runtime.Default))
	if want == "" {
		want = "local"
	}
	active, ok := backends[want]
	if !ok {
		if want == "sge" {
			return nil, errors.New("runtime.default is sge but runtime.sge is absent or disabled")
		}
		return nil, fmt.Errorf("runtime.default %q is not a known backend (want local|sge)", cfg.Runtime.Default)
	}
	return newManagerWithBackends(cfg, pool, backends, active)
}

func newPortPool(cfg *config.Config) *ports.Pool {
	return ports.New(cfg.Runtime.Local.PortRange[0], cfg.Runtime.Local.PortRange[1])
}

// newManager builds a single-backend manager. It is the test/embedded seam: the
// backend is registered under its own name and is also the active one.
func newManager(cfg *config.Config, backend runtime.Backend) (*Manager, error) {
	return newManagerWithBackends(cfg, newPortPool(cfg), map[string]runtime.Backend{backend.Name(): backend}, backend)
}

func newManagerWithBackends(cfg *config.Config, pool *ports.Pool, backends map[string]runtime.Backend, active runtime.Backend) (*Manager, error) {
	store, err := state.New(cfg.Server.DataDir)
	if err != nil {
		return nil, err
	}
	return &Manager{
		cfg:      cfg,
		store:    store,
		ports:    pool,
		backends: backends,
		active:   active,
		guard:    state.NewMemGuard(),
		last:     map[string]time.Time{},
		handles:  map[string]runtime.Handle{},
		logf:     func(string, ...any) {},
	}, nil
}

// SetLogf routes the manager's warnings (renewal failures, lease warnings) to
// the process log. Without it they are recorded on the instance only.
func (m *Manager) SetLogf(f func(format string, args ...any)) {
	if f == nil {
		return
	}
	m.mu.Lock()
	m.logf = f
	m.mu.Unlock()
}

func (m *Manager) logfTo(format string, args ...any) {
	m.mu.Lock()
	f := m.logf
	m.mu.Unlock()
	if f != nil {
		f(format, args...)
	}
}

// backendFor routes by the record first, so probe/stop/renew/adopt keep working
// for an instance started by a backend that is no longer the default.
func (m *Manager) backendFor(inst *model.Instance) runtime.Backend {
	if inst != nil && inst.Backend != "" {
		if b, ok := m.backends[inst.Backend]; ok {
			return b
		}
	}
	return m.active
}

func (m *Manager) Store() *state.Store { return m.store }

func (m *Manager) Ensure(ctx context.Context, req Request) (*model.Instance, error) {
	if strings.TrimSpace(req.User) == "" {
		return nil, errors.New("user_id is required")
	}
	distName := req.Distribution
	if distName == "" {
		distName = m.cfg.Runtime.DefaultDistribution
	}
	dist, ok := m.cfg.Distributions[distName]
	if !ok {
		return nil, fmt.Errorf("unknown distribution %q", distName)
	}
	unlock := m.guard.Lock(req.User)
	defer unlock()

	id := model.InstanceID(req.User, distName)
	existing, err := m.store.Load(req.User, distName)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.State.Active() && !req.Force {
		return existing, nil
	}
	if existing != nil && existing.State.Active() && req.Force {
		if err := m.stopLocked(ctx, existing); err != nil {
			return nil, err
		}
	}

	slug := model.Slugify(req.User)
	home := filepath.Join(m.cfg.Server.DataDir, "homes", slug)
	workspace := req.Workspace
	if workspace == "" {
		workspace = filepath.Join(m.cfg.Server.DataDir, "workspaces", slug)
	}
	workspace = strings.TrimPrefix(workspace, "path:")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, err
	}
	if err := distribution.Materialize(home, dist); err != nil {
		return nil, fmt.Errorf("materialize profile: %w", err)
	}
	if err := ensureDefaultWorkspaceLink(workspace); err != nil {
		return nil, fmt.Errorf("default workspace link: %w", err)
	}

	port, err := m.ports.Acquire(id)
	if err != nil {
		return nil, err
	}
	portOK := false
	defer func() {
		if !portOK {
			m.ports.Release(port)
		}
	}()

	backend := m.active
	backendName := backend.Name()

	gatewayHost, gatewayPort, err := net.SplitHostPort(m.cfg.Server.Listen)
	if err != nil {
		return nil, fmt.Errorf("server.listen: %w", err)
	}
	_ = gatewayHost
	publicHost := publicAuthority(slug, m.cfg.Routing.HostSuffix, gatewayPort, m.cfg.Routing.PublicScheme)
	trustedHost := fmt.Sprintf("%s.%s", slug, m.cfg.Routing.HostSuffix)
	inc := randomID()
	controlDir := filepath.Join(m.cfg.Server.DataDir, "control", id)
	unitName := "dw-" + slug + "-" + inc[:8] + ".service"
	logPath := filepath.Join(m.cfg.Server.DataDir, "logs", id+".log")
	if backendName == "sge" {
		// A job script's name is not a unit name, and the job log must live
		// inside the one directory the container is guaranteed to see.
		unitName = strings.TrimSuffix(unitName, ".service")
		logPath = filepath.Join(controlDir, "job.log")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	patchContent := runtime.RuntimePatch(workspace)

	inst := &model.Instance{
		ID:           id,
		User:         req.User,
		Slug:         slug,
		Distribution: distName,
		DistVersion:  dist.Version,
		State:        model.StatePending,
		Backend:      backendName,
		Incarnation:  inc,
		UnitName:     unitName,
		Home:         home,
		Workspace:    workspace,
		Profile:      dist.ProfileName,
		PublicHost:   publicHost,
		TrustedHost:  trustedHost,
		Port:         port,
		ControlDir:   controlDir,
		LogPath:      logPath,
		CPU:          dist.Defaults.CPU,
		Memory:       dist.Defaults.Memory,
		Walltime:     dist.Defaults.Walltime,
		GPU:          dist.Defaults.GPU,
		IdleTTL:      dist.Defaults.IdleTTL,
		CreatedAt:    time.Now().UTC(),
	}
	if inst.CPU == 0 {
		inst.CPU = 2
	}
	if inst.Memory == "" {
		inst.Memory = "4Gi"
	}
	if inst.IdleTTL == "" {
		inst.IdleTTL = "30m"
	}
	if err := m.store.Save(inst); err != nil {
		return nil, err
	}

	spec := agent.Spec{
		InstanceID:   id,
		Incarnation:  inc,
		UnitName:     inst.UnitName,
		Profile:      dist.ProfileName,
		DshBin:       m.cfg.Runtime.DSHBin,
		Home:         home,
		Workspace:    workspace,
		DistPath:     dist.Path,
		DistVersion:  dist.Version,
		ControlDir:   controlDir,
		LogPath:      logPath,
		PublicURL:    fmt.Sprintf("%s://%s/", m.cfg.Routing.PublicScheme, publicHost),
		TrustedHost:  trustedHost,
		Port:         port,
		PatchContent: patchContent,
		Heartbeat:    m.cfg.Runtime.Heartbeat,
		ReadyTimeout: m.cfg.Runtime.ReadyTimeout,
		CPU:          inst.CPU,
		Memory:       inst.Memory,
		Walltime:     inst.Walltime,
		GPU:          inst.GPU,
	}
	h, err := backend.Start(ctx, spec)
	if err != nil {
		m.fail(inst, err)
		return nil, err
	}
	inst.State = model.StateStarting
	inst.PID = h.PID()
	inst.PIDStart = h.PIDStart()
	inst.Limiter = h.Limiter()
	inst.Command = h.Command()
	if backendName == "sge" {
		// The job id is the only identity that survives this process; the pid
		// lives on a compute node.
		inst.BackendRef = h.Ref()
	} else if h.Limiter() == "systemd" {
		inst.BackendRef = h.Ref()
	} else {
		inst.BackendRef = ""
	}
	inst.StartedAt = time.Now().UTC()
	if lh, ok := h.(runtime.LeaseHandle); ok {
		inst.LeaseExpiresAt = lh.LeaseExpiresAt(inst.StartedAt)
	}
	if err := m.store.Save(inst); err != nil {
		_ = h.Stop(ctx)
		return nil, err
	}
	// A scheduler-backed backend decides for itself whether Ensure may block: on
	// SGE a job can wait hours for a slot, and a platform request must not.
	budget := m.cfg.ReadyTimeout()
	if p, ok := backend.(runtime.Policy); ok {
		budget = p.LaunchBudget()
	}
	ready, err := h.Endpoint(ctx, budget)
	if errors.Is(err, runtime.ErrNotYetRunning) {
		// Submitted, not placed. Return a queued record: the platform gets its
		// ticket immediately and the fast tick promotes the instance.
		inst.State = model.StateQueued
		inst.Error = "queued"
		if err := m.store.Save(inst); err != nil {
			return nil, err
		}
		portOK = true
		m.mu.Lock()
		m.handles[id] = h
		m.last[id] = inst.StartedAt
		m.mu.Unlock()
		return inst, nil
	}
	if err != nil {
		_ = h.Stop(ctx)
		m.fail(inst, err)
		return nil, err
	}
	inst.State = model.StateRunning
	inst.Endpoint = fmt.Sprintf("%s:%d", ready.Host, ready.Port)
	inst.Node = ready.Node
	// A backend may publish a port the control plane did not allocate (SGE's
	// directDial mode dials the job host itself); the pooled port was never
	// bound, so it is released rather than leaked.
	if ready.Port > 0 && ready.Port != inst.Port {
		m.ports.Release(inst.Port)
		inst.Port = ready.Port
		_ = m.ports.Reserve(inst.Port, id)
	}
	// The far side of the tunnel is worth keeping: it is what an operator needs
	// to look at qstat/ssh by hand.
	if d, ok := h.(runtime.EndpointDescriber); ok {
		if node, jobPort := d.DescribeEndpoint(); node != "" && jobPort > 0 {
			inst.Node = node
			inst.JobPort = jobPort
			inst.RemoteEndpoint = fmt.Sprintf("%s:%d", node, jobPort)
		}
	}
	_ = os.Remove(filepath.Join(m.cfg.Server.DataDir, "runtime", id, "dsh-session.json"))
	inst.ReadyAt = time.Now().UTC()
	inst.LastActiveAt = inst.ReadyAt
	inst.Error = ""
	if err := m.store.Save(inst); err != nil {
		return nil, err
	}
	portOK = true
	m.mu.Lock()
	m.handles[id] = h
	m.last[id] = inst.LastActiveAt
	m.mu.Unlock()
	return inst, nil
}

func (m *Manager) Stop(ctx context.Context, user, dist string) error {
	inst, err := m.store.Load(user, dist)
	if err != nil {
		return err
	}
	if inst == nil {
		return errors.New("unknown instance")
	}
	unlock := m.guard.Lock(user)
	defer unlock()
	return m.stopLocked(ctx, inst)
}

func (m *Manager) stopLocked(ctx context.Context, inst *model.Instance) error {
	if inst.State.Terminal() {
		return nil
	}
	inst.State = model.StateStopping
	_ = m.store.Save(inst)
	if err := m.stopBackend(ctx, inst); err != nil {
		// A failed qdel/systemctl stop is not a stopped instance. Keep the
		// record active and the port reserved so the next tick can retry; the
		// caller sees the real error instead of a silent leak.
		inst.Error = "stop failed: " + err.Error()
		_ = m.store.Save(inst)
		return err
	}
	m.ports.Release(inst.Port)
	_ = os.Remove(filepath.Join(m.cfg.Server.DataDir, "runtime", inst.ID, "dsh-session.json"))
	inst.State = model.StateStopped
	inst.EndedAt = time.Now().UTC()
	_ = m.store.Save(inst)
	m.mu.Lock()
	delete(m.handles, inst.ID)
	delete(m.last, inst.ID)
	m.mu.Unlock()
	return nil
}

// stopBackend prefers the in-process handle (fast, owns the wait channel) and
// falls back to the backend for records adopted after a control-plane restart.
func (m *Manager) stopBackend(ctx context.Context, inst *model.Instance) error {
	m.mu.Lock()
	h := m.handles[inst.ID]
	m.mu.Unlock()
	if h != nil {
		return h.Stop(ctx)
	}
	return m.backendFor(inst).Stop(ctx, inst)
}

func (m *Manager) fail(inst *model.Instance, err error) {
	inst.State = model.StateFailed
	inst.Error = err.Error()
	inst.EndedAt = time.Now().UTC()
	_ = m.store.Save(inst)
	m.ports.Release(inst.Port)
}

func (m *Manager) ByHost(host string) (*model.Instance, error) {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	suffix := "." + strings.ToLower(m.cfg.Routing.HostSuffix)
	if !strings.HasSuffix(hostname, suffix) {
		return nil, nil
	}
	slug := strings.TrimSuffix(hostname, suffix)
	if slug == "" || strings.Contains(slug, ".") {
		return nil, nil
	}
	list, err := m.store.List()
	if err != nil {
		return nil, err
	}
	for _, inst := range list {
		if inst.Slug == slug {
			return inst, nil
		}
	}
	return nil, nil
}

func (m *Manager) Touch(inst *model.Instance) {
	m.mu.Lock()
	m.last[inst.ID] = time.Now().UTC()
	m.mu.Unlock()
}

func (m *Manager) LastActive(inst *model.Instance) time.Time {
	m.mu.Lock()
	t := m.last[inst.ID]
	m.mu.Unlock()
	if t.After(inst.LastActiveAt) {
		return t
	}
	return inst.LastActiveAt
}

func (m *Manager) Token(inst *model.Instance) (string, error) {
	return rendezvous.Read(inst.ControlDir, "token")
}

func (m *Manager) List() ([]*model.Instance, error) {
	return m.store.List()
}

func (m *Manager) Reconcile(ctx context.Context) error {
	list, err := m.store.List()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, inst := range list {
		if !inst.State.Active() {
			continue
		}
		m.reconcileOne(ctx, inst, now)
		// A port is only held once the instance can serve: reserving one for a
		// launch that has not bound it yet would starve the pool.
		switch inst.State {
		case model.StateQueued, model.StateSuspended, model.StateRunning:
			if inst.Port > 0 {
				_ = m.ports.Reserve(inst.Port, inst.ID)
			}
		}
	}
	return nil
}

func (m *Manager) reconcileOne(ctx context.Context, inst *model.Instance, now time.Time) {
	backend := m.backendFor(inst)
	obs, probeErr := backend.Probe(ctx, inst)
	if probeErr == nil && obs.Gone() {
		// A scheduler's explicit failure state (SGE Eqw and friends) is a
		// decision, not a clean stop: keep it visible as `failed` so the user
		// sees why the job never ran. A plain process/unit that disappeared is
		// settled as stopped (the agent may have exited cleanly).
		if obs.Phase == runtime.PhaseFailed && schedulerBacked(backend) {
			m.settleFailed(ctx, inst, obs.Describe(), now)
		} else {
			m.settleStopped(inst, obs.Describe(), now)
		}
		return
	}
	if probeErr == nil && obs.Alive() {
		if obs.PID > 0 && obs.PID != inst.PID {
			inst.PID = obs.PID
			_ = m.store.Save(inst)
		}
		if obs.PIDStart > 0 && obs.PIDStart != inst.PIDStart {
			inst.PIDStart = obs.PIDStart
			_ = m.store.Save(inst)
		}
		if obs.Node != "" && obs.Node != inst.Node {
			inst.Node = obs.Node
			_ = m.store.Save(inst)
		}
	}

	if probeErr == nil {
		switch obs.Phase {
		case runtime.PhaseQueued:
			// Waiting for a slot is a legitimate place to be for hours. The only
			// timeout is the explicitly configured queueTimeout.
			if m.queueExpired(inst, now) {
				if err := m.stopBackend(ctx, inst); err != nil {
					inst.Error = "queue timeout, stop failed: " + err.Error()
					_ = m.store.Save(inst)
					return
				}
				m.ports.Release(inst.Port)
				inst.State = model.StateFailed
				inst.Error = fmt.Sprintf("job did not start within %s (still queued)", m.cfg.Runtime.SGE.QueueTimeout)
				inst.EndedAt = now
				_ = m.store.Save(inst)
				return
			}
			if inst.State != model.StateQueued || obs.Note != inst.Error {
				inst.State = model.StateQueued
				inst.Error = obs.Note
				_ = m.store.Save(inst)
			}
			return
		case runtime.PhaseSuspended:
			// Suspended is alive: SGE paused the job and may resume it. Do not
			// settle, do not reap, do not resubmit.
			if inst.State != model.StateSuspended || obs.Note != inst.Error {
				inst.State = model.StateSuspended
				inst.Error = obs.Note
				_ = m.store.Save(inst)
			}
			return
		}
	}

	if inst.State == model.StateStarting && !schedulerBacked(backend) && m.stuckStarting(inst, now) {
		m.failStuckStarting(ctx, inst, now)
		return
	}
	// Promote a starting or queued instance once its own record says ready. This
	// is the restart path too: Adopt re-establishes reachability instead of
	// trusting a dead route from an earlier control plane.
	if inst.State == model.StateStarting || inst.State == model.StateQueued {
		if probeErr == nil && obs.Alive() {
			if _, err := m.promote(ctx, inst); err != nil && !errors.Is(err, runtime.ErrNotYetRunning) {
				m.logfTo("promote %s: %v", inst.ID, err)
			}
		}
	}
}

// schedulerBacked reports whether the backend's own scheduler owns placement and
// the lifecycle ceiling. The stuck-starting escape hatch does not apply there: a
// job can sit in a queue for six hours and is governed by queueTimeout and h_rt.
func schedulerBacked(b runtime.Backend) bool {
	_, ok := b.(runtime.Policy)
	return ok
}

// promote is the single implementation of "observe ready.json -> make the
// endpoint reachable -> persist". Ensure uses the handle; this path uses Adopt.
func (m *Manager) promote(ctx context.Context, inst *model.Instance) (bool, error) {
	r, err := rendezvous.ReadReady(inst.ControlDir)
	if err != nil || r.Incarnation != inst.Incarnation || r.State != "ready" || r.Port <= 0 {
		return false, nil
	}
	host, port := r.Host, r.Port
	backend := m.backendFor(inst)
	m.mu.Lock()
	handle := m.handles[inst.ID]
	m.mu.Unlock()
	switch {
	case handle != nil && schedulerBacked(backend):
		// This process submitted the job (it is queued, not adopted). Only a
		// scheduler-backed handle may be polled with a zero budget; the local one
		// would block for the full ready timeout inside a reconcile tick.
		ready, err := handle.Endpoint(ctx, 0)
		if err != nil {
			return false, err
		}
		host, port = ready.Host, ready.Port
	case handle == nil:
		if adopter, ok := backend.(runtime.Adopter); ok {
			h, p, adoptErr := adopter.Adopt(ctx, inst)
			if adoptErr != nil {
				return false, adoptErr
			}
			host, port = h, p
		}
	}
	if handle != nil {
		if d, ok := handle.(runtime.EndpointDescriber); ok {
			if node, jobPort := d.DescribeEndpoint(); node != "" && jobPort > 0 {
				inst.Node = node
				inst.JobPort = jobPort
				inst.RemoteEndpoint = fmt.Sprintf("%s:%d", node, jobPort)
			}
		}
	}
	inst.State = model.StateRunning
	inst.Endpoint = fmt.Sprintf("%s:%d", host, port)
	if port > 0 && port != inst.Port {
		// Same rule as Ensure: an endpoint the control plane did not allocate
		// replaces the pooled port instead of leaving it reserved for nothing.
		m.ports.Release(inst.Port)
		inst.Port = port
		_ = m.ports.Reserve(inst.Port, inst.ID)
	}
	inst.ReadyAt = r.PublishedAt
	inst.LastActiveAt = time.Now().UTC()
	inst.Error = ""
	if err := m.store.Save(inst); err != nil {
		return false, err
	}
	return true, nil
}

// settleStopped records a confirmed death and releases everything the instance
// held.
func (m *Manager) settleFailed(ctx context.Context, inst *model.Instance, reason string, now time.Time) {
	_ = m.stopBackend(ctx, inst)
	inst.State = model.StateFailed
	inst.Error = reason
	inst.EndedAt = now
	_ = m.store.Save(inst)
	m.ports.Release(inst.Port)
	m.mu.Lock()
	delete(m.handles, inst.ID)
	m.mu.Unlock()
}

func (m *Manager) settleStopped(inst *model.Instance, reason string, now time.Time) {
	inst.State = model.StateStopped
	inst.Error = reason
	inst.EndedAt = now
	_ = m.store.Save(inst)
	m.ports.Release(inst.Port)
	m.mu.Lock()
	delete(m.handles, inst.ID)
	m.mu.Unlock()
}

// queueExpired reports whether an explicitly configured queueTimeout has
// elapsed. The default is 0 (wait forever): a slot wait is not an error.
func (m *Manager) queueExpired(inst *model.Instance, now time.Time) bool {
	s := m.cfg.Runtime.SGE
	if s == nil || inst.StartedAt.IsZero() {
		return false
	}
	d, err := time.ParseDuration(s.QueueTimeout)
	if err != nil || d <= 0 {
		return false
	}
	return now.Sub(inst.StartedAt) > d
}

// stuckStarting is the escape hatch for a wedged launch: Ensure would return
// this active record forever, locking the user out.
func (m *Manager) stuckStarting(inst *model.Instance, now time.Time) bool {
	if inst.StartedAt.IsZero() {
		return false
	}
	limit := m.cfg.ReadyTimeout() + m.cfg.StopGrace()
	if limit <= 0 {
		limit = 110 * time.Second
	}
	return now.Sub(inst.StartedAt) > limit
}

func (m *Manager) failStuckStarting(ctx context.Context, inst *model.Instance, now time.Time) {
	unlock := m.guard.Lock(inst.User)
	defer unlock()
	_ = m.stopBackend(ctx, inst)
	m.ports.Release(inst.Port)
	inst.State = model.StateFailed
	inst.Error = fmt.Sprintf("ready timeout after %s", m.cfg.ReadyTimeout())
	inst.EndedAt = now
	_ = m.store.Save(inst)
	m.mu.Lock()
	delete(m.handles, inst.ID)
	m.mu.Unlock()
}

func (m *Manager) Sweep(ctx context.Context) error {
	list, err := m.store.List()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, inst := range list {
		// Queued and suspended instances are not idle, they are waiting or
		// paused. Killing them would force a re-queue for no benefit.
		if inst.State != model.StateRunning {
			continue
		}
		backend := m.backendFor(inst)
		if policy, ok := backend.(runtime.Policy); ok && !policy.IdleReapAllowed() {
			// On a scheduler the lifecycle ceiling is h_rt, and renewal exists so
			// that an idle instance does not have to be reaped. Sites can still
			// opt in with sge.idleReap: true.
			continue
		}
		ttl, err := time.ParseDuration(inst.IdleTTL)
		if err != nil || ttl <= 0 {
			continue
		}
		if now.Sub(m.LastActive(inst)) > ttl {
			unlock := m.guard.Lock(inst.User)
			_ = m.stopLocked(ctx, inst)
			unlock()
		}
	}
	return nil
}

// RenewTick extends scheduler leases and warns about the ones it cannot move.
// Every step is optional: a backend without Renewer or LeaseWarner is skipped,
// never guessed at.
func (m *Manager) RenewTick(ctx context.Context) error {
	list, err := m.store.List()
	if err != nil {
		return err
	}
	for _, inst := range list {
		if inst.State != model.StateRunning && inst.State != model.StateSuspended {
			continue
		}
		backend := m.backendFor(inst)
		if renewer, ok := backend.(runtime.Renewer); ok {
			renewed, renewErr := renewer.Renew(ctx, inst)
			if renewErr != nil {
				// A renewal failure is a countdown, not a kill.
				m.logfTo("renew %s: %v", inst.ID, renewErr)
			} else if renewed {
				if err := m.store.Save(inst); err != nil {
					m.logfTo("renew %s: save: %v", inst.ID, err)
				}
				m.logfTo("renewed %s until %s", inst.ID, inst.LeaseExpiresAt.UTC().Format(time.RFC3339))
			}
		}
		warner, ok := backend.(runtime.LeaseWarner)
		if !ok {
			continue
		}
		if !inst.LeaseWarnedAt.IsZero() {
			continue // already warned for this lease window; renewal re-arms it
		}
		if msg, warn := warner.LeaseWarning(inst); warn {
			inst.LeaseWarnedAt = time.Now().UTC()
			inst.Error = msg
			if err := m.store.Save(inst); err != nil {
				m.logfTo("lease warn %s: save: %v", inst.ID, err)
			}
			m.logfTo("lease %s: %s", inst.ID, msg)
		}
	}
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) {
	// Deliberately do not stop instances: a control-plane restart must adopt
	// them. Only explicit Stop/Sweep stops dsh.
}

func ensureDefaultWorkspaceLink(workspace string) error {
	dir := filepath.Join(workspace, "deepseek-harness")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	link := filepath.Join(dir, "default-workspace")
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return nil
	}
	return os.Symlink(workspace, link)
}

func publicAuthority(slug, suffix, port, scheme string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return slug + "." + suffix
	}
	return fmt.Sprintf("%s.%s:%s", slug, suffix, port)
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
