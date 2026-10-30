package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/deepwarper/internal/model"
)

var ErrLocked = errors.New("lock is held by another process")

// ErrConflict means a record changed between the caller's read and its
// read-modify-write. The caller must reload and retry.
var ErrConflict = errors.New("instance record changed concurrently")

type Store struct {
	Dir string

	mu   sync.Mutex
	keys map[string]*sync.Mutex
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "instances"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "users"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, keys: map[string]*sync.Mutex{}}, nil
}

// InstancePath is the current, collision-free path for (user, dist).
func (s *Store) InstancePath(user, dist string) string {
	return filepath.Join(s.Dir, "instances", model.InstanceID(user, dist)+".yaml")
}

// LegacyInstancePath is the historical "<user>-<dist>.yaml" path. It is only
// returned when that name is a single safe filename component.
func (s *Store) LegacyInstancePath(user, dist string) string {
	id := model.LegacyInstanceID(user, dist)
	if !model.IsSafeFileComponent(id) {
		return ""
	}
	return filepath.Join(s.Dir, "instances", id+".yaml")
}

// Load reads the record for (user, dist), trying the current path first and
// the legacy path second. A record whose stored identity does not match the
// request is ignored.
func (s *Store) Load(user, dist string) (*model.Instance, error) {
	inst, _, err := s.loadCurrent(user, dist)
	return inst, err
}

// LoadByID reads instances/<id>.yaml. Unsafe ids are rejected before they are
// used as a path component.
func (s *Store) LoadByID(id string) (*model.Instance, error) {
	if !model.IsSafeFileComponent(id) {
		return nil, fmt.Errorf("state: unsafe instance id %q", id)
	}
	return s.loadPath(filepath.Join(s.Dir, "instances", id+".yaml"))
}

func (s *Store) loadPath(path string) (*model.Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var inst model.Instance
	if err := yaml.Unmarshal(data, &inst); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if inst.Revision == 0 {
		// Records written before revisions existed are at revision 1.
		inst.Revision = 1
	}
	return &inst, nil
}

func (s *Store) loadCurrent(user, dist string) (*model.Instance, string, error) {
	paths := []string{s.InstancePath(user, dist)}
	if legacy := s.LegacyInstancePath(user, dist); legacy != "" {
		paths = append(paths, legacy)
	}
	for _, path := range paths {
		inst, err := s.loadPath(path)
		if err != nil {
			return nil, "", err
		}
		if inst == nil {
			continue
		}
		if inst.User != user || inst.Distribution != dist {
			continue
		}
		return inst, path, nil
	}
	return nil, "", nil
}

// Save upserts the record under its own ID. It preserves a caller-supplied
// (legacy) ID and only derives one when the record has none. Revision bumps
// monotonically from whatever is on disk; last writer wins, so lifecycle code
// must use Update instead.
func (s *Store) Save(inst *model.Instance) error {
	if inst == nil {
		return errors.New("state: nil instance")
	}
	if inst.ID == "" {
		inst.ID = model.InstanceID(inst.User, inst.Distribution)
	}
	if !model.IsSafeFileComponent(inst.ID) {
		return fmt.Errorf("state: unsafe instance id %q", inst.ID)
	}
	path := filepath.Join(s.Dir, "instances", inst.ID+".yaml")
	cur, err := s.loadPath(path)
	if err != nil {
		return err
	}
	next := uint64(1)
	if cur != nil {
		next = cur.Revision + 1
		if next <= inst.Revision {
			next = inst.Revision + 1
		}
	} else if inst.Revision > 0 {
		next = inst.Revision + 1
	}
	inst.Revision = next
	data, err := yaml.Marshal(inst)
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

// Update is the only read-modify-write API. The caller's Revision must match
// the record on disk (0 means "must not exist yet"); fn then edits that fresh
// copy and the result is written back with a bumped revision. Access is
// serialized by an in-process per-key mutex plus an advisory file lock, so a
// second `dw serve` sharing the data directory cannot interleave a write.
func (s *Store) Update(inst *model.Instance, fn func(*model.Instance) error) (*model.Instance, error) {
	if inst == nil {
		return nil, errors.New("state: nil instance")
	}
	key := inst.User + "\x00" + inst.Distribution
	unlock := s.lockKey(key)
	defer unlock()

	lock, err := acquire(s.instLockPath(key), true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	cur, path, err := s.loadCurrent(inst.User, inst.Distribution)
	if err != nil {
		return nil, err
	}
	curRev := uint64(0)
	if cur != nil {
		curRev = cur.Revision
	}
	if curRev != inst.Revision {
		return nil, ErrConflict
	}
	if cur == nil {
		clone := *inst
		clone.Revision = 0
		cur = &clone
		path = s.InstancePath(inst.User, inst.Distribution)
	}
	if fn != nil {
		if err := fn(cur); err != nil {
			return nil, err
		}
	}
	if cur.ID == "" {
		cur.ID = model.InstanceID(cur.User, cur.Distribution)
	}
	if !model.IsSafeFileComponent(cur.ID) {
		return nil, fmt.Errorf("state: unsafe instance id %q", cur.ID)
	}
	if path == "" {
		path = s.InstancePath(cur.User, cur.Distribution)
	}
	cur.Revision = inst.Revision + 1
	data, err := yaml.Marshal(cur)
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(path, data, 0o600); err != nil {
		return nil, err
	}
	return cur, nil
}

// List returns every record, deduplicated by (user, distribution) and
// preferring the current filename when both spellings exist.
func (s *Store) List() ([]*model.Instance, error) {
	dir := filepath.Join(s.Dir, "instances")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		path string
		inst *model.Instance
	}
	groups := map[string][]candidate{}
	var order []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		inst, err := s.loadPath(path)
		if err != nil {
			return nil, err
		}
		if inst == nil {
			continue
		}
		key := inst.User + "\x00" + inst.Distribution
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], candidate{path: path, inst: inst})
	}
	var out []*model.Instance
	for _, key := range order {
		group := groups[key]
		chosen := group[0]
		preferred := s.InstancePath(chosen.inst.User, chosen.inst.Distribution)
		for _, c := range group {
			if c.path == preferred {
				chosen = c
				break
			}
		}
		out = append(out, chosen.inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) LoadUser(user string) (*model.User, error) {
	path := filepath.Join(s.Dir, "users", model.Slugify(user)+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var u model.User
	if err := yaml.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) SaveUser(u *model.User) error {
	path := filepath.Join(s.Dir, "users", model.Slugify(u.UserID)+".yaml")
	data, err := yaml.Marshal(u)
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

// lockKey returns the in-process mutex for a record key.
func (s *Store) lockKey(key string) func() {
	s.mu.Lock()
	if s.keys == nil {
		s.keys = map[string]*sync.Mutex{}
	}
	m := s.keys[key]
	if m == nil {
		m = &sync.Mutex{}
		s.keys[key] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Store) instLockPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.Dir, "locks", "inst-"+hex.EncodeToString(sum[:8])+".lock")
}

// LockDataDir takes the process-wide control-plane lock. A second owner gets
// ErrLocked. The lock file records the owning pid and start time for
// diagnostics and is released with Release.
func LockDataDir(dir string) (*FileLock, error) {
	l, err := acquire(filepath.Join(dir, "locks", "serve.lock"), false)
	if err != nil {
		return nil, err
	}
	if l.f != nil {
		if err := l.f.Truncate(0); err == nil {
			if _, err := l.f.Seek(0, 0); err == nil {
				_, _ = fmt.Fprintf(l.f, "pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
				_ = l.f.Sync()
			}
		}
	}
	return l, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type FileLock struct {
	f    *os.File
	path string
}

// Acquire takes a non-blocking exclusive flock.
func Acquire(path string) (*FileLock, error) {
	return acquire(path, false)
}

func acquire(path string, block bool) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX | syscall.LOCK_NB
	if block {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if !block && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &FileLock{f: f, path: path}, nil
}

func (l *FileLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// MemGuard serializes work in one process even on filesystems where flock is
// not reliable (some NFS mounts). It is not a substitute for the file lock.
type MemGuard struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func NewMemGuard() *MemGuard { return &MemGuard{m: map[string]*sync.Mutex{}} }

func (g *MemGuard) Lock(key string) func() {
	g.mu.Lock()
	m := g.m[key]
	if m == nil {
		m = &sync.Mutex{}
		g.m[key] = m
	}
	g.mu.Unlock()
	m.Lock()
	return m.Unlock
}
