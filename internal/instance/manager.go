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
	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/distribution"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/pathutil"
	"github.com/seqyuan/deepwarper/internal/ports"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
	"github.com/seqyuan/deepwarper/internal/runtime/local"
	"github.com/seqyuan/deepwarper/internal/state"
)

var (
	// ErrInvalidRequest is a malformed platform request (identity, workspace,
	// distribution). The gateway maps it to HTTP 400.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrUnknownUser means login mode is on and the account does not exist.
	ErrUnknownUser = errors.New("unknown user")
	// ErrUnknownInstance means there is no record for (user, distribution).
	ErrUnknownInstance = errors.New("unknown instance")
	// ErrBusy is matched by BusyError.
	ErrBusy = errors.New("instance is busy")

	errStaleSnapshot = errors.New("instance record changed concurrently")
	errSuperseded    = errors.New("instance launch was superseded")
)

// BusyError reports an instance in a state that cannot serve a new launch. The
// gateway maps it to HTTP 409 with Retry-After.
type BusyError struct {
	User         string
	Distribution string
	State        model.State
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("instance %s/%s is %s", e.User, e.Distribution, e.State)
}

func (e *BusyError) Is(target error) bool { return target == ErrBusy }

type Request struct {
	User         string
	DisplayName  string
	Distribution string
	Workspace    string
	Force        bool
}

// handleEntry ties an in-process handle to the incarnation that created it, so
// a stale handle can never stop a newer process.
type handleEntry struct {
	id string
	h  runtime.Handle
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
	// handles is keyed by incarnation: see handleEntry.
	handles map[string]handleEntry
	logf    func(format string, args ...any)
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
	backend := local.New(LocalOptions(cfg))
	return newManagerWithBackends(cfg, pool, map[string]runtime.Backend{"local": backend}, backend)
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
		handles:  map[string]handleEntry{},
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

// guardKey is the MemGuard key. Every lifecycle path uses (user, distribution)
// so Ensure/Stop/Sweep/Reconcile never nest or miss each other.
func guardKey(user, dist string) string { return user + "\x00" + dist }

// portOwner scopes a port reservation to one incarnation. A stale record can
// therefore never free a port that a newer incarnation took over.
func portOwner(inst *model.Instance) string {
	return inst.ID + "/" + inst.Incarnation
}

// update wraps Store.Update and turns a revision conflict into a sentinel the
// lifecycle code can handle. The caller must already hold the guard.
func (m *Manager) update(inst *model.Instance, fn func(*model.Instance) error) (*model.Instance, error) {
	cur, err := m.store.Update(inst, fn)
	if errors.Is(err, state.ErrConflict) {
		return nil, errStaleSnapshot
	}
	return cur, err
}

func (m *Manager) setHandle(inst *model.Instance, h runtime.Handle) {
	if inst == nil || h == nil || inst.Incarnation == "" {
		return
	}
	m.mu.Lock()
	// Drop any stale handle for the same instance first; only the current
	// incarnation may be stopped through the in-process map.
	for inc, entry := range m.handles {
		if entry.id == inst.ID && inc != inst.Incarnation {
			delete(m.handles, inc)
		}
	}
	m.handles[inst.Incarnation] = handleEntry{id: inst.ID, h: h}
	m.mu.Unlock()
}

func (m *Manager) handleFor(inst *model.Instance) runtime.Handle {
	if inst == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.handles[inst.Incarnation]
	if !ok || entry.id != inst.ID {
		return nil
	}
	return entry.h
}

func (m *Manager) dropHandle(inc string) {
	m.mu.Lock()
	delete(m.handles, inc)
	m.mu.Unlock()
}

func (m *Manager) Ensure(ctx context.Context, req Request) (*model.Instance, error) {
	// Identity, account and distribution are validated before anything is
	// created on disk.
	if strings.TrimSpace(req.User) == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrInvalidRequest)
	}
	if err := model.ValidateUserID(req.User); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	distName := req.Distribution
	if distName == "" {
		distName = m.cfg.Runtime.DefaultDistribution
	}
	if err := model.ValidateDistKey(distName); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	dist, ok := m.cfg.Distributions[distName]
	if !ok {
		return nil, fmt.Errorf("%w: unknown distribution %q", ErrInvalidRequest, distName)
	}
	if m.loginEnabled() {
		u, err := auth.Load(m.cfg.Auth.UsersDir, req.User)
		if err != nil || u == nil || !u.Active() {
			if err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %v", ErrUnknownUser, err)
			}
			return nil, fmt.Errorf("%w: %q", ErrUnknownUser, req.User)
		}
	}

	unlock := m.guard.Lock(guardKey(req.User, distName))
	defer unlock()

	existing, err := m.store.Load(req.User, distName)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.State.Active() && req.Force {
		if err := m.stopLocked(ctx, existing); err != nil {
			return nil, err
		}
		existing, err = m.store.Load(req.User, distName)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	if existing != nil && !req.Force {
		switch existing.State {
		case model.StateStarting, model.StateRunning:
			return existing, nil
		case model.StateStopping:
			return nil, &BusyError{User: req.User, Distribution: distName, State: existing.State}
		case model.StatePending:
			if !m.pendingExpired(existing, now) {
				return existing, nil
			}
			// Stale pending: quiesce it before reusing the record.
		}
	}
	// The record is about to be overwritten. Whatever process it still
	// describes must be gone first, or the new incarnation would share (or
	// lose track of) it. The returned record carries the revision the reuse
	// update must be based on.
	if existing != nil {
		existing, err = m.quiesceLocked(ctx, existing)
		if err != nil {
			return nil, err
		}
	}

	id := model.InstanceID(req.User, distName)
	if existing != nil && model.IsSafeFileComponent(existing.ID) {
		id = existing.ID
	}
	slug, err := m.slugFor(req.User, existing)
	if err != nil {
		return nil, err
	}
	requested := strings.TrimPrefix(req.Workspace, "path:")
	workspace, err := m.resolveWorkspace(existing, slug, requested)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace: %v", ErrInvalidRequest, err)
	}
	home, err := m.resolveHome(existing, slug)
	if err != nil {
		return nil, fmt.Errorf("%w: home: %v", ErrInvalidRequest, err)
	}
	controlDir, err := m.resolveControlDir(existing, id)
	if err != nil {
		return nil, fmt.Errorf("%w: control dir: %v", ErrInvalidRequest, err)
	}
	logPath, err := m.resolveLogPath(existing, id)
	if err != nil {
		return nil, fmt.Errorf("%w: log path: %v", ErrInvalidRequest, err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	if err := distribution.Materialize(home, dist); err != nil {
		return nil, fmt.Errorf("materialize profile: %w", err)
	}
	if err := m.prepareHome(home, req.User); err != nil {
		return nil, fmt.Errorf("prepare home: %w", err)
	}
	if err := ensureDefaultWorkspaceLink(workspace, workspace); err != nil {
		return nil, fmt.Errorf("default workspace link: %w", err)
	}
	patchContent, err := runtime.RuntimePatch(workspace)
	if err != nil {
		return nil, err
	}

	inc := randomID()
	owner := id + "/" + inc
	port, err := m.ports.Acquire(owner)
	if err != nil {
		return nil, err
	}
	portOK := false
	defer func() {
		if !portOK {
			m.ports.ReleaseOwned(port, owner)
		}
	}()

	backend := m.active
	// The public authority uses routing.publicPort (falling back to the
	// listen port), not the loopback listen port: behind a 443 proxy the
	// visible URL must not be host:7658.
	publicHost := publicAuthority(slug, m.cfg.Routing.HostSuffix, m.cfg.EffectivePublicPort(), m.cfg.Routing.PublicScheme)
	trustedHost := fmt.Sprintf("%s.%s", slug, m.cfg.Routing.HostSuffix)
	unitName := "dw-" + slug + "-" + inc[:8] + ".service"
	inst := &model.Instance{
		ID:           id,
		User:         req.User,
		Slug:         slug,
		Distribution: distName,
		DistVersion:  dist.Version,
		State:        model.StatePending,
		Backend:      backend.Name(),
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
		CreatedAt:    now,
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
	if existing == nil {
		if err := m.store.Save(inst); err != nil {
			return nil, err
		}
	} else {
		// Reuse the record (and its ID/paths) instead of writing a second file.
		saved, err := m.update(existing, func(cur *model.Instance) error {
			*cur = *inst
			return nil
		})
		if err != nil {
			return nil, err
		}
		inst = saved
		// The previous incarnation's reservation is no longer ours.
		if existing.Port > 0 {
			m.ports.ReleaseOwned(existing.Port, portOwner(existing))
		}
	}
	portOK = true

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
	if err := m.attachSandbox(&spec, req.User); err != nil {
		return nil, m.abortLocked(ctx, inst, nil, err)
	}
	h, err := backend.Start(ctx, spec)
	if err != nil {
		return nil, m.abortLocked(ctx, inst, nil, err)
	}
	inst.State = model.StateStarting
	inst.PID = h.PID()
	inst.PIDStart = h.PIDStart()
	inst.Limiter = h.Limiter()
	inst.Command = h.Command()
	if h.Limiter() == "systemd" {
		inst.BackendRef = h.Ref()
	} else {
		inst.BackendRef = ""
	}
	inst.StartedAt = time.Now().UTC()
	started, err := m.update(inst, func(cur *model.Instance) error {
		cur.State = model.StateStarting
		cur.PID = inst.PID
		cur.PIDStart = inst.PIDStart
		cur.Limiter = inst.Limiter
		cur.Command = append([]string{}, inst.Command...)
		cur.BackendRef = inst.BackendRef
		cur.StartedAt = inst.StartedAt
		cur.Error = ""
		return nil
	})
	if err != nil {
		if errors.Is(err, errStaleSnapshot) {
			fresh, lerr := m.store.Load(req.User, distName)
			if lerr != nil {
				return nil, m.abortLocked(ctx, inst, h, lerr)
			}
			if fresh != nil && fresh.Incarnation == inc &&
				(fresh.State == model.StateStarting || fresh.State == model.StateRunning) {
				inst = fresh
			} else {
				// Another incarnation owns the record now: stop the process we
				// started and drop this launch's port claim. The record itself
				// belongs to the winner and must not be touched.
				_ = m.stopAttempt(ctx, inst, h)
				m.dropHandle(inc)
				m.ports.ReleaseOwned(port, owner)
				return nil, fmt.Errorf("%w: %s/%s changed during launch", errSuperseded, req.User, distName)
			}
		} else {
			return nil, m.abortLocked(ctx, inst, h, err)
		}
	} else {
		inst = started
	}

	// TODO(launch-cancellation): the readiness wait holds the guard, so a
	// second Ensure for the same user queues for up to readyTimeout. A future
	// two-phase launch should release the guard here and signal waiters.
	ready, err := h.Endpoint(ctx, m.cfg.ReadyTimeout())
	if err != nil {
		return nil, m.abortLocked(ctx, inst, h, err)
	}
	running, err := m.update(inst, func(cur *model.Instance) error {
		cur.State = model.StateRunning
		cur.Node = ready.Node
		cur.Endpoint = fmt.Sprintf("%s:%d", ready.Host, ready.Port)
		if ready.Port > 0 && ready.Port != cur.Port {
			m.ports.ReleaseOwned(cur.Port, portOwner(cur))
			cur.Port = ready.Port
			if rerr := m.ports.Reserve(cur.Port, portOwner(cur)); rerr != nil {
				return rerr
			}
		}
		cur.ReadyAt = time.Now().UTC()
		cur.LastActiveAt = cur.ReadyAt
		cur.Error = ""
		return nil
	})
	if err != nil {
		if errors.Is(err, errStaleSnapshot) {
			fresh, lerr := m.store.Load(req.User, distName)
			if lerr == nil && fresh != nil && fresh.Incarnation == inc && fresh.State == model.StateRunning {
				running = fresh
			} else {
				return nil, m.abortLocked(ctx, inst, h, fmt.Errorf("%w: %s/%s changed during launch", errSuperseded, req.User, distName))
			}
		} else {
			return nil, m.abortLocked(ctx, inst, h, err)
		}
	}
	if running.Port != port {
		// A conflicting writer already recorded a different port for this
		// incarnation; the launch-time reservation is no longer needed.
		m.ports.ReleaseOwned(port, owner)
	}
	m.cleanupRuntime(running.ID)
	m.setHandle(running, h)
	m.mu.Lock()
	m.last[running.ID] = running.LastActiveAt
	m.mu.Unlock()
	return running, nil
}

// abortLocked is the single failure path after a Pending record exists. It
// marks the record Stopping, stops the backend on a detached context, and only
// then declares Failed and releases the port. A failed stop keeps Stopping so
// Reconcile can retry. The caller must hold the guard.
func (m *Manager) abortLocked(ctx context.Context, inst *model.Instance, h runtime.Handle, cause error) error {
	if inst == nil {
		return cause
	}
	stopping, err := m.update(inst, func(cur *model.Instance) error {
		cur.State = model.StateStopping
		cur.Error = cause.Error()
		cur.LastStopAttemptAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		// The record moved on (superseded or written elsewhere). Do not clobber
		// it, but the process this launch started must not survive.
		if h != nil {
			_ = m.stopAttempt(ctx, inst, h)
		}
		m.ports.ReleaseOwned(inst.Port, portOwner(inst))
		m.dropHandle(inst.Incarnation)
		return cause
	}
	if serr := m.stopAttempt(ctx, stopping, h); serr != nil {
		stopping.Error = cause.Error() + "; stop failed: " + serr.Error()
		stopping.StopAttempts++
		stopping.LastStopAttemptAt = time.Now().UTC()
		if _, uerr := m.update(stopping, func(cur *model.Instance) error {
			cur.State = model.StateStopping
			cur.Error = stopping.Error
			cur.StopAttempts = stopping.StopAttempts
			cur.LastStopAttemptAt = stopping.LastStopAttemptAt
			return nil
		}); uerr != nil {
			m.logfTo("abort %s: record update: %v", stopping.ID, uerr)
		}
		if h != nil {
			m.setHandle(stopping, h)
		}
		return cause
	}
	if _, uerr := m.update(stopping, func(cur *model.Instance) error {
		cur.State = model.StateFailed
		cur.Error = cause.Error()
		cur.EndedAt = time.Now().UTC()
		return nil
	}); uerr != nil {
		m.logfTo("abort %s: record update: %v", stopping.ID, uerr)
	}
	m.ports.ReleaseOwned(stopping.Port, portOwner(stopping))
	m.dropHandle(stopping.Incarnation)
	m.cleanupRuntime(stopping.ID)
	return cause
}

// quiesceLocked makes a record safe to overwrite. A confirmed Stopped record
// needs no work; a stale Pending launch and a legacy Failed record may still
// own a process, so that process is stopped first. A failed stop keeps the
// record Stopping (never Failed) and returns the error, so Reconcile can
// retry instead of orphaning the process. The caller must hold the guard and
// must reuse the returned record for the subsequent update.
func (m *Manager) quiesceLocked(ctx context.Context, inst *model.Instance) (*model.Instance, error) {
	switch inst.State {
	case model.StateStopped:
		return inst, nil
	case model.StatePending:
		stopping, err := m.update(inst, func(cur *model.Instance) error {
			cur.State = model.StateStopping
			cur.Error = "superseded by new launch"
			cur.StopAttempts++
			cur.LastStopAttemptAt = time.Now().UTC()
			return nil
		})
		if err != nil {
			return nil, err
		}
		if serr := m.stopAttempt(ctx, stopping, nil); serr != nil {
			msg := "reuse stop failed: " + serr.Error()
			if _, uerr := m.update(stopping, func(cur *model.Instance) error {
				cur.State = model.StateStopping
				cur.Error = msg
				cur.LastStopAttemptAt = time.Now().UTC()
				return nil
			}); uerr != nil {
				m.logfTo("reuse %s: record update: %v", stopping.ID, uerr)
			}
			return nil, errors.New(msg)
		}
		stopped, err := m.update(stopping, func(cur *model.Instance) error {
			cur.State = model.StateStopped
			cur.Error = "superseded by new launch"
			cur.EndedAt = time.Now().UTC()
			return nil
		})
		if err != nil {
			return nil, err
		}
		m.ports.ReleaseOwned(stopped.Port, portOwner(stopped))
		m.dropHandle(stopped.Incarnation)
		return stopped, nil
	case model.StateFailed:
		if serr := m.stopAttempt(ctx, inst, nil); serr != nil {
			msg := "reuse stop failed: " + serr.Error()
			if _, uerr := m.update(inst, func(cur *model.Instance) error {
				cur.State = model.StateStopping
				cur.Error = msg
				cur.LastStopAttemptAt = time.Now().UTC()
				return nil
			}); uerr != nil {
				m.logfTo("reuse %s: record update: %v", inst.ID, uerr)
			}
			return nil, errors.New(msg)
		}
		// Failed stays Failed: the record is only a placeholder for the new
		// launch, and the (legacy) process behind it is now gone.
		m.ports.ReleaseOwned(inst.Port, portOwner(inst))
		m.dropHandle(inst.Incarnation)
		return inst, nil
	default:
		return inst, nil
	}
}

// Stop reloads the record under the guard, so the caller's snapshot can never
// stop a newer incarnation.
func (m *Manager) Stop(ctx context.Context, user, dist string) error {
	if dist == "" {
		dist = m.cfg.Runtime.DefaultDistribution
	}
	unlock := m.guard.Lock(guardKey(user, dist))
	defer unlock()
	inst, err := m.store.Load(user, dist)
	if err != nil {
		return err
	}
	if inst == nil {
		return fmt.Errorf("%w: %s/%s", ErrUnknownInstance, user, dist)
	}
	return m.stopLocked(ctx, inst)
}

// stopLocked transitions to Stopping via CAS, stops on a detached context, and
// only marks Stopped after the backend confirmed the stop. A failure keeps
// Stopping plus the port and the handle so a later retry can finish the job.
func (m *Manager) stopLocked(ctx context.Context, inst *model.Instance) error {
	if inst.State.Terminal() {
		return nil
	}
	stopping, err := m.update(inst, func(cur *model.Instance) error {
		cur.State = model.StateStopping
		cur.Error = ""
		cur.StopAttempts = 0
		cur.LastStopAttemptAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	if err := m.stopAttempt(ctx, stopping, nil); err != nil {
		stopping.Error = "stop failed: " + err.Error()
		stopping.StopAttempts++
		stopping.LastStopAttemptAt = time.Now().UTC()
		if _, uerr := m.update(stopping, func(cur *model.Instance) error {
			cur.State = model.StateStopping
			cur.Error = stopping.Error
			cur.StopAttempts = stopping.StopAttempts
			cur.LastStopAttemptAt = stopping.LastStopAttemptAt
			return nil
		}); uerr != nil {
			m.logfTo("stop %s: record update: %v", stopping.ID, uerr)
		}
		return err
	}
	return m.settleStopped(stopping, "", time.Now().UTC())
}

// stopAttempt prefers the in-process handle (fast, owns the wait channel) and
// falls back to the backend for records adopted after a control-plane restart.
// It always runs detached from the caller's cancellation: an HTTP timeout must
// not abandon a kill.
func (m *Manager) stopAttempt(ctx context.Context, inst *model.Instance, h runtime.Handle) error {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	stopCtx, cancel := context.WithTimeout(base, m.cfg.StopGrace()+30*time.Second)
	defer cancel()
	if h == nil {
		h = m.handleFor(inst)
	}
	if h != nil {
		return h.Stop(stopCtx)
	}
	return m.backendFor(inst).Stop(stopCtx, inst)
}

// settleStopped records a confirmed stop (or a Gone process) and releases the
// port of that incarnation only.
func (m *Manager) settleStopped(inst *model.Instance, reason string, now time.Time) error {
	settled, err := m.update(inst, func(cur *model.Instance) error {
		cur.State = model.StateStopped
		cur.Error = reason
		cur.EndedAt = now
		return nil
	})
	if err != nil {
		return err
	}
	m.ports.ReleaseOwned(settled.Port, portOwner(settled))
	m.dropHandle(settled.Incarnation)
	m.cleanupRuntime(settled.ID)
	return nil
}

// cleanupRuntime removes the per-instance runtime scratch file. Safe IDs only.
func (m *Manager) cleanupRuntime(id string) {
	if !model.IsSafeFileComponent(id) {
		return
	}
	_ = os.Remove(filepath.Join(m.cfg.Server.DataDir, "runtime", id, "dsh-session.json"))
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

// ByUser returns the user's running instance, or another of their records
// when none is running. A user with no record yields nil, nil.
func (m *Manager) ByUser(user string) (*model.Instance, error) {
	list, err := m.store.List()
	if err != nil {
		return nil, err
	}
	var fallback *model.Instance
	for _, inst := range list {
		if inst.User != user {
			continue
		}
		if inst.State == model.StateRunning {
			return inst, nil
		}
		if fallback == nil {
			fallback = inst
		}
	}
	return fallback, nil
}

// seedLastActive gives a Running record an in-memory activity timestamp once.
// It never overwrites a timestamp recorded by a real request.
func (m *Manager) seedLastActive(id string, now time.Time) {
	if id == "" {
		return
	}
	m.mu.Lock()
	if _, ok := m.last[id]; !ok {
		m.last[id] = now
	}
	m.mu.Unlock()
}

func (m *Manager) Touch(inst *model.Instance) {
	if inst == nil {
		return
	}
	m.mu.Lock()
	m.last[inst.ID] = time.Now().UTC()
	m.mu.Unlock()
}

func (m *Manager) LastActive(inst *model.Instance) time.Time {
	if inst == nil {
		return time.Time{}
	}
	m.mu.Lock()
	t := m.last[inst.ID]
	m.mu.Unlock()
	if t.After(inst.LastActiveAt) {
		return t
	}
	return inst.LastActiveAt
}

// Token reads the dsh token, refusing a control directory that is not under
// <dataDir>/control.
func (m *Manager) Token(inst *model.Instance) (string, error) {
	if err := m.checkControlDir(inst); err != nil {
		return "", err
	}
	return rendezvous.Read(inst.ControlDir, "token")
}

func (m *Manager) checkControlDir(inst *model.Instance) error {
	if inst == nil {
		return errors.New("instance is required")
	}
	real, err := pathutil.ResolveForCreate(inst.ControlDir)
	if err != nil {
		return err
	}
	root, err := pathutil.ResolveForCreate(filepath.Join(m.cfg.Server.DataDir, "control"))
	if err != nil {
		return err
	}
	if !pathutil.Within(real, root) || filepath.Clean(real) == filepath.Clean(root) {
		return fmt.Errorf("control dir %q is outside %q", inst.ControlDir, root)
	}
	return nil
}

func (m *Manager) List() ([]*model.Instance, error) {
	return m.store.List()
}

// Reconcile reserves every active record's port, probes outside the guard, then
// reloads and applies transitions. It never transitions Pending -> Starting:
// Ensure owns that edge, and doing it here would race an in-flight launch.
func (m *Manager) Reconcile(ctx context.Context) error {
	list, err := m.store.List()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	m.reservePorts(list)
	for _, inst := range list {
		if !inst.State.Active() {
			continue
		}
		m.reconcileOne(ctx, inst, now)
	}
	return nil
}

// reservePorts claims ports for all active records, Running before the
// transient states. A launch that has not bound its port yet still owns it, so
// another Ensure cannot double-book it after a control-plane restart.
func (m *Manager) reservePorts(list []*model.Instance) {
	for _, state := range []model.State{
		model.StateRunning, model.StateStarting, model.StateStopping, model.StatePending,
	} {
		for _, inst := range list {
			if inst.State != state || inst.Port <= 0 {
				continue
			}
			if err := m.ports.Reserve(inst.Port, portOwner(inst)); err != nil {
				m.logfTo("reserve port %d for %s: %v", inst.Port, inst.ID, err)
				m.noteError(inst, "port reservation failed: "+err.Error())
			}
		}
	}
}

// noteError attaches a diagnostic to the record. It reloads under the guard so
// a scan cannot overwrite a launch that is in flight.
func (m *Manager) noteError(inst *model.Instance, note string) {
	unlock := m.guard.Lock(guardKey(inst.User, inst.Distribution))
	defer unlock()
	fresh, err := m.store.Load(inst.User, inst.Distribution)
	if err != nil || fresh == nil {
		return
	}
	if _, err := m.update(fresh, func(cur *model.Instance) error {
		// Append instead of replacing: a stop failure or orphan marker must
		// survive the next scan's port note.
		if !strings.Contains(cur.Error, note) {
			if cur.Error == "" {
				cur.Error = note
			} else {
				cur.Error += "; " + note
			}
		}
		return nil
	}); err != nil {
		m.logfTo("note %s: %v", inst.ID, err)
	}
}

func (m *Manager) reconcileOne(ctx context.Context, snap *model.Instance, now time.Time) {
	backend := m.backendFor(snap)
	obs, probeErr := backend.Probe(ctx, snap)

	unlock := m.guard.Lock(guardKey(snap.User, snap.Distribution))
	defer unlock()

	inst, err := m.store.Load(snap.User, snap.Distribution)
	if err != nil {
		m.logfTo("reconcile %s: %v", snap.ID, err)
		return
	}
	if inst == nil || inst.Incarnation != snap.Incarnation || inst.State != snap.State {
		// The record moved while the probe ran; the next tick sees the new one.
		return
	}

	if inst.State == model.StateRunning {
		// A record adopted after a control-plane restart has no in-memory
		// activity entry yet. Without seeding it, an old persisted
		// LastActiveAt lets the next Sweep stop the instance immediately.
		m.seedLastActive(inst.ID, now)
	}

	if probeErr == nil && obs.Gone() && inst.State != model.StatePending {
		// A Pending record that never became a process is an aborted launch, not
		// a settled instance; reconcilePending reports it as Failed.
		if err := m.settleStopped(inst, obs.Describe(), now); err != nil {
			m.logfTo("settle %s: %v", inst.ID, err)
		}
		return
	}
	if probeErr == nil && obs.Alive() && obs.PID > 0 &&
		(obs.PID != inst.PID || (obs.PIDStart > 0 && obs.PIDStart != inst.PIDStart)) {
		if adopted, aerr := m.update(inst, func(cur *model.Instance) error {
			cur.PID = obs.PID
			if obs.PIDStart > 0 {
				cur.PIDStart = obs.PIDStart
			}
			return nil
		}); aerr == nil && adopted != nil {
			inst = adopted
		} else {
			m.logfTo("adopt pid for %s: %v", inst.ID, aerr)
		}
	}

	switch inst.State {
	case model.StatePending:
		m.reconcilePending(ctx, inst, obs, probeErr, now)
	case model.StateStarting:
		m.reconcileStarting(ctx, inst, obs, probeErr, now)
	case model.StateRunning:
		// Ports were reserved by reservePorts; a Gone running instance was
		// settled above.
	case model.StateStopping:
		if err := m.retryStoppingLocked(ctx, inst, now); err != nil {
			m.logfTo("retry stop %s: %v", inst.ID, err)
		}
	}
}

func (m *Manager) reconcilePending(ctx context.Context, inst *model.Instance, obs runtime.Observation, probeErr error, now time.Time) {
	if ready, err := rendezvous.ReadReady(inst.ControlDir); err == nil && m.readyMatches(inst, ready) {
		if err := m.promote(inst, ready); err != nil {
			m.logfTo("promote %s: %v", inst.ID, err)
		}
		return
	}
	if probeErr == nil && obs.Gone() {
		_ = m.abortLocked(ctx, inst, nil, fmt.Errorf("instance exited before ready (%s)", obs.Describe()))
		return
	}
	if m.pendingExpired(inst, now) {
		_ = m.abortLocked(ctx, inst, nil, fmt.Errorf("launch did not start within %s", m.pendingLimit()))
	}
}

func (m *Manager) reconcileStarting(ctx context.Context, inst *model.Instance, obs runtime.Observation, probeErr error, now time.Time) {
	if ready, err := rendezvous.ReadReady(inst.ControlDir); err == nil && m.readyMatches(inst, ready) {
		if err := m.promote(inst, ready); err != nil {
			m.logfTo("promote %s: %v", inst.ID, err)
		}
		return
	}
	if m.stuckStarting(inst, now) {
		_ = m.abortLocked(ctx, inst, nil, fmt.Errorf("ready timeout after %s", m.cfg.ReadyTimeout()))
		return
	}
	if probeErr == nil && !obs.Alive() && obs.Gone() {
		_ = m.abortLocked(ctx, inst, nil, fmt.Errorf("instance exited before ready (%s)", obs.Describe()))
	}
}

// readyMatches reports whether ready.json belongs to this incarnation. Legacy
// records without an incarnation are accepted only when the publication time
// and recorded PID agree with the launch.
func (m *Manager) readyMatches(inst *model.Instance, ready *rendezvous.Ready) bool {
	if ready == nil || ready.State != "ready" || ready.Port <= 0 || ready.Incarnation == "" {
		return false
	}
	if inst.Incarnation != "" {
		return ready.Incarnation == inst.Incarnation
	}
	if !ready.PublishedAt.IsZero() && ready.PublishedAt.Before(effectiveStart(inst)) {
		return false
	}
	if ready.PID > 0 && inst.PID > 0 && ready.PID != inst.PID {
		return false
	}
	return true
}

// promote records a ready.json that belongs to this incarnation. It runs under
// the guard and uses CAS so a superseded launch cannot be promoted.
func (m *Manager) promote(inst *model.Instance, ready *rendezvous.Ready) error {
	if ready == nil || ready.State != "ready" || ready.Port <= 0 {
		return nil
	}
	if inst.Incarnation != "" && ready.Incarnation != inst.Incarnation {
		return nil
	}
	updated, err := m.update(inst, func(cur *model.Instance) error {
		oldPort, oldOwner := cur.Port, portOwner(cur)
		if cur.Incarnation == "" {
			// Adopt the incarnation published by the launch.
			cur.Incarnation = ready.Incarnation
		}
		cur.State = model.StateRunning
		if ready.Node != "" {
			cur.Node = ready.Node
		}
		if ready.Port != oldPort {
			m.ports.ReleaseOwned(oldPort, oldOwner)
			cur.Port = ready.Port
			if rerr := m.ports.Reserve(cur.Port, portOwner(cur)); rerr != nil {
				return rerr
			}
		}
		cur.Endpoint = fmt.Sprintf("%s:%d", ready.Host, ready.Port)
		cur.ReadyAt = ready.PublishedAt
		if cur.ReadyAt.IsZero() {
			cur.ReadyAt = time.Now().UTC()
		}
		cur.LastActiveAt = time.Now().UTC()
		cur.Error = ""
		return nil
	})
	if err != nil {
		return err
	}
	m.ownPort(updated)
	return nil
}

// ownPort re-owns a record's port after an incarnation was adopted from a
// legacy record, whose reservation is keyed by the empty incarnation.
func (m *Manager) ownPort(inst *model.Instance) {
	if inst == nil || inst.Port <= 0 {
		return
	}
	owner := portOwner(inst)
	cur, ok := m.ports.Owner(inst.Port)
	if ok && cur == owner {
		return
	}
	if ok && cur != inst.ID+"/" && !strings.HasPrefix(cur, inst.ID+"/") {
		// Someone else holds it; do not steal. The caller holds the guard, so
		// only log here (noteError would re-lock and deadlock).
		m.logfTo("port %d for %s is held by %s", inst.Port, inst.ID, cur)
		return
	}
	if ok {
		m.ports.ReleaseOwned(inst.Port, cur)
	}
	_ = m.ports.Reserve(inst.Port, owner)
}

// orphanSuspected is appended once when a stop keeps failing.
const orphanSuspected = "orphan suspected"

func (m *Manager) retryStoppingLocked(ctx context.Context, inst *model.Instance, now time.Time) error {
	interval := m.cfg.ScanInterval()
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	last := inst.LastStopAttemptAt
	if last.IsZero() {
		last = effectiveStart(inst)
	}
	if !last.IsZero() && now.Sub(last) < interval {
		return nil
	}
	if err := m.stopAttempt(ctx, inst, nil); err != nil {
		attempts := inst.StopAttempts + 1
		msg := "stop failed: " + err.Error()
		// Once suspected, the marker stays on every later retry: dropping it
		// would hide an orphan that is still alive.
		if attempts >= 5 || strings.Contains(inst.Error, orphanSuspected) {
			msg += "; " + orphanSuspected
		}
		if _, uerr := m.update(inst, func(cur *model.Instance) error {
			cur.State = model.StateStopping
			cur.Error = msg
			cur.StopAttempts = attempts
			cur.LastStopAttemptAt = now
			return nil
		}); uerr != nil {
			m.logfTo("retry stop %s: record update: %v", inst.ID, uerr)
		}
		return err
	}
	return m.settleStopped(inst, "", now)
}

func (m *Manager) stuckStarting(inst *model.Instance, now time.Time) bool {
	start := effectiveStart(inst)
	if start.IsZero() {
		return false
	}
	return now.Sub(start) > m.cfg.ReadyTimeout()+m.cfg.StopGrace()
}

func (m *Manager) pendingLimit() time.Duration {
	limit := m.cfg.ReadyTimeout() + m.cfg.StopGrace()
	if limit < 60*time.Second {
		limit = 60 * time.Second
	}
	return limit
}

func (m *Manager) pendingExpired(inst *model.Instance, now time.Time) bool {
	start := effectiveStart(inst)
	if start.IsZero() {
		return true
	}
	return now.Sub(start) > m.pendingLimit()
}

func effectiveStart(inst *model.Instance) time.Time {
	if inst == nil {
		return time.Time{}
	}
	if !inst.StartedAt.IsZero() {
		return inst.StartedAt
	}
	return inst.CreatedAt
}

func (m *Manager) Sweep(ctx context.Context) error {
	list, err := m.store.List()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, snap := range list {
		if snap.State != model.StateRunning {
			continue
		}
		ttl, err := time.ParseDuration(snap.IdleTTL)
		if err != nil || ttl <= 0 {
			continue
		}
		if now.Sub(m.LastActive(snap)) <= ttl {
			continue
		}
		unlock := m.guard.Lock(guardKey(snap.User, snap.Distribution))
		inst, lerr := m.store.Load(snap.User, snap.Distribution)
		if lerr != nil {
			unlock()
			m.logfTo("sweep %s: %v", snap.ID, lerr)
			continue
		}
		if inst == nil || inst.Incarnation != snap.Incarnation || inst.State != model.StateRunning {
			unlock()
			continue
		}
		if now.Sub(m.LastActive(inst)) <= ttl {
			unlock()
			continue
		}
		if err := m.stopLocked(ctx, inst); err != nil {
			m.logfTo("sweep %s: %v", inst.ID, err)
		}
		unlock()
	}
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) {
	// Deliberately do not stop instances: a control-plane restart must adopt
	// them. Only explicit Stop/Sweep stops dsh.
}

func (m *Manager) loginEnabled() bool {
	return strings.TrimSpace(m.cfg.Auth.UsersDir) != ""
}

// slugFor reuses a record's slug, otherwise maps the identity to a canonical or
// stable local slug. A slug already owned by another record gets the stable
// hash suffix so two users never share a host.
func (m *Manager) slugFor(user string, existing *model.Instance) (string, error) {
	slug := ""
	if existing != nil && model.IsSafeFileComponent(existing.Slug) {
		slug = existing.Slug
	}
	if slug == "" {
		slug = model.LocalSlug(user)
	}
	list, err := m.store.List()
	if err != nil {
		return "", err
	}
	for _, inst := range list {
		if inst.Slug != slug || inst.User == user {
			continue
		}
		slug = model.StableSlug(user)
		break
	}
	if !model.IsSafeFileComponent(slug) {
		return "", fmt.Errorf("cannot derive a safe slug for %q", user)
	}
	return slug, nil
}

// workspaceRoots is the implicit per-site workspaces directory plus the
// configured extra roots.
func (m *Manager) workspaceRoots() ([]string, error) {
	implicit := filepath.Join(m.cfg.Server.DataDir, "workspaces")
	if err := os.MkdirAll(implicit, 0o700); err != nil {
		return nil, err
	}
	roots := []string{implicit}
	for _, root := range m.cfg.Server.WorkspaceRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// resolveWorkspace returns the physical workspace path. A requested path is
// only accepted inside an allowed root; the workspace itself may not be a root,
// and inside the implicit root it must stay in the per-user subtree.
func (m *Manager) resolveWorkspace(existing *model.Instance, slug, requested string) (string, error) {
	roots, err := m.workspaceRoots()
	if err != nil {
		return "", err
	}
	implicit := filepath.Join(m.cfg.Server.DataDir, "workspaces")
	perUser := filepath.Join(implicit, slug)
	if err := os.MkdirAll(perUser, 0o700); err != nil {
		return "", err
	}

	candidate := requested
	if candidate == "" && existing != nil && existing.Workspace != "" {
		candidate = existing.Workspace
	}
	if candidate == "" {
		candidate = perUser
	}
	if err := validateHostPathArg(candidate); err != nil {
		return "", err
	}
	if err := local.ValidateUnitPath("workspace", candidate); err != nil {
		return "", err
	}
	real, err := pathutil.EnsureUnder(candidate, roots)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		realRoot, rerr := pathutil.Resolve(root)
		if rerr != nil {
			return "", fmt.Errorf("workspace root %q: %w", root, rerr)
		}
		if real == realRoot {
			return "", fmt.Errorf("workspace %q is a mount root, not a workspace", candidate)
		}
	}
	// Reuse of an existing record keeps its historical location; a new or
	// requested workspace under the implicit root must be the user's own
	// subtree so one user cannot point at another user's data.
	if requested != "" && pathutil.Within(real, implicit) {
		realPerUser, rerr := pathutil.Resolve(perUser)
		if rerr != nil {
			return "", rerr
		}
		if !pathutil.Within(real, realPerUser) {
			return "", fmt.Errorf("workspace %q must be inside %q", candidate, realPerUser)
		}
	}
	return real, nil
}

func (m *Manager) resolveHome(existing *model.Instance, slug string) (string, error) {
	root := filepath.Join(m.cfg.Server.DataDir, "homes")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	candidate := filepath.Join(root, slug)
	if existing != nil && existing.Home != "" {
		candidate = existing.Home
	}
	if err := validateHostPathArg(candidate); err != nil {
		return "", err
	}
	if err := local.ValidateUnitPath("home", candidate); err != nil {
		return "", err
	}
	real, err := pathutil.EnsureUnder(candidate, []string{root})
	if err != nil {
		return "", err
	}
	realRoot, err := pathutil.Resolve(root)
	if err != nil {
		return "", err
	}
	if real == realRoot {
		return "", fmt.Errorf("home %q is the homes root", candidate)
	}
	return real, nil
}

func (m *Manager) resolveControlDir(existing *model.Instance, id string) (string, error) {
	root := filepath.Join(m.cfg.Server.DataDir, "control")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	candidate := filepath.Join(root, id)
	if existing != nil && existing.ControlDir != "" {
		candidate = existing.ControlDir
	}
	if err := validateHostPathArg(candidate); err != nil {
		return "", err
	}
	if err := local.ValidateUnitPath("control dir", candidate); err != nil {
		return "", err
	}
	real, err := pathutil.EnsureUnder(candidate, []string{root})
	if err != nil {
		return "", err
	}
	realRoot, err := pathutil.Resolve(root)
	if err != nil {
		return "", err
	}
	if real == realRoot {
		return "", fmt.Errorf("control dir %q is the control root", candidate)
	}
	return real, nil
}

func (m *Manager) resolveLogPath(existing *model.Instance, id string) (string, error) {
	root := filepath.Join(m.cfg.Server.DataDir, "logs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	candidate := filepath.Join(root, id+".log")
	if existing != nil && existing.LogPath != "" {
		candidate = existing.LogPath
	}
	if err := validateHostPathArg(candidate); err != nil {
		return "", err
	}
	if err := local.ValidateUnitPath("log path", candidate); err != nil {
		return "", err
	}
	real, err := pathutil.ResolveForCreate(candidate)
	if err != nil {
		return "", err
	}
	realRoot, err := pathutil.Resolve(root)
	if err != nil {
		return "", err
	}
	if !pathutil.Within(real, realRoot) || real == realRoot {
		return "", fmt.Errorf("log path %q is outside %q", candidate, realRoot)
	}
	return real, nil
}

// validateHostPathArg rejects path spellings that no exec argv or systemd
// property can carry. Containment is checked separately.
func validateHostPathArg(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path is empty")
	}
	if strings.ContainsAny(p, "\x00\r\n%\"'") {
		return fmt.Errorf("path %q contains a NUL, CR, LF, percent or quote", p)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be absolute", p)
	}
	if filepath.Clean(p) == string(filepath.Separator) {
		return errors.New("path must not be /")
	}
	return nil
}

func ensureDefaultWorkspaceLink(workspace, target string) error {
	dir := filepath.Join(workspace, "deepseek-harness")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	link := filepath.Join(dir, "default-workspace")
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if got, err := os.Readlink(link); err == nil && got == target {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return os.Symlink(target, link)
}

// publicAuthority builds the browser-facing host. The default port for the
// scheme is omitted; an unknown port (0) also yields the bare host.
func publicAuthority(slug, suffix string, publicPort int, scheme string) string {
	if publicPort <= 0 {
		return slug + "." + suffix
	}
	if (scheme == "https" && publicPort == 443) || (scheme == "http" && publicPort == 80) {
		return slug + "." + suffix
	}
	return fmt.Sprintf("%s.%s:%d", slug, suffix, publicPort)
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
