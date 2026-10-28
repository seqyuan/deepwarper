package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/deepwarper/internal/model"
)

var ErrLocked = errors.New("lock is held by another process")

type Store struct {
	Dir string
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
	return &Store{Dir: dir}, nil
}

func (s *Store) InstancePath(user, dist string) string {
	return filepath.Join(s.Dir, "instances", model.InstanceID(user, dist)+".yaml")
}

func (s *Store) Load(user, dist string) (*model.Instance, error) {
	path := s.InstancePath(user, dist)
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
	return &inst, nil
}

func (s *Store) LoadByID(id string) (*model.Instance, error) {
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
	return &inst, nil
}

func (s *Store) Save(inst *model.Instance) error {
	path := s.InstancePath(inst.User, inst.Distribution)
	data, err := yaml.Marshal(inst)
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o600)
}

func (s *Store) List() ([]*model.Instance, error) {
	dir := filepath.Join(s.Dir, "instances")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*model.Instance
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		inst, err := s.loadPath(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if inst != nil {
			out = append(out, inst)
		}
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

func Acquire(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
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
