package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Mount is one virtual mount owned by a user. Kind is path or bucket.
// Mode is ro or rw.
type Mount struct {
	Name   string `yaml:"name"`
	Kind   string `yaml:"kind"`
	Host   string `yaml:"host,omitempty"`
	Remote string `yaml:"remote,omitempty"`
	Prefix string `yaml:"prefix,omitempty"`
	Target string `yaml:"target"`
	Mode   string `yaml:"mode"`
}

// User is the on-disk account record. Name comes from the file name.
type User struct {
	Name string `yaml:"-"`
	Auth struct {
		PasswordHash   string `yaml:"password_hash"`
		SessionVersion int    `yaml:"session_version"`
		// Deleted tombstones an account instead of removing the file, so the
		// advanced SessionVersion survives a delete/recreate cycle and old
		// dw_session cookies stay invalid.
		Deleted bool `yaml:"deleted,omitempty"`
	} `yaml:"auth"`
	Mounts  []Mount  `yaml:"mounts"`
	Plugins []string `yaml:"plugins"`
}

// Active reports whether the account may log in and use its instance. A
// tombstoned account keeps its file (and generation) but no credentials, and a
// record without a password hash is not a usable account either.
func (u *User) Active() bool {
	return u != nil && !u.Auth.Deleted && u.Auth.PasswordHash != ""
}

var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// dummyHash is compared when the user does not exist so the response time
// does not reveal which of "unknown" and "bad password" happened.
var dummyHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	dummyHash = h
}

// IsValidUsername reports whether name matches the account name rule.
func IsValidUsername(name string) bool {
	return usernameRE.MatchString(name)
}

// Create writes <dir>/<name>.yaml with a bcrypt hash. The plaintext password
// is never stored.
func Create(dir, name, password string) error {
	if !IsValidUsername(name) {
		return fmt.Errorf("auth: invalid username %q", name)
	}
	if password == "" {
		return errors.New("auth: empty password")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, name+".yaml")
	if existing, lerr := Load(dir, name); lerr == nil {
		if existing.Auth.Deleted {
			// Recreate over a tombstone: the generation keeps advancing so
			// sessions issued before the delete never come back to life.
			hash, herr := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if herr != nil {
				return herr
			}
			existing.Auth.PasswordHash = string(hash)
			existing.Auth.Deleted = false
			existing.Mounts = []Mount{}
			existing.Plugins = []string{}
			if existing.Auth.SessionVersion < 2 {
				existing.Auth.SessionVersion = 2
			}
			return Save(dir, existing)
		}
		return fmt.Errorf("auth: user %q already exists", name)
	} else if !os.IsNotExist(lerr) {
		return lerr
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u := &User{Name: name, Mounts: []Mount{}, Plugins: []string{}}
	u.Auth.PasswordHash = string(hash)
	u.Auth.SessionVersion = 1
	data, err := yaml.Marshal(u)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("auth: user %q already exists", name)
		}
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	return os.Chmod(path, 0o600)
}

// SetPassword replaces the stored hash for an existing user.
func SetPassword(dir, name, password string) error {
	if password == "" {
		return errors.New("auth: empty password")
	}
	u, err := Load(dir, name)
	if err != nil {
		return err
	}
	if !u.Active() {
		return fmt.Errorf("auth: user %q not found", name)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Auth.PasswordHash = string(hash)
	u.Auth.SessionVersion++
	if u.Auth.SessionVersion < 2 {
		u.Auth.SessionVersion = 2
	}
	return Save(dir, u)
}

// Delete tombstones the user record: the file stays (with an advanced
// SessionVersion and no credentials) so delete+recreate cannot revive an old
// session or hand the new account the old generation.
func Delete(dir, name string) error {
	if !IsValidUsername(name) {
		return fmt.Errorf("auth: invalid username %q", name)
	}
	u, err := Load(dir, name)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("auth: user %q not found", name)
		}
		return err
	}
	if !u.Active() {
		return fmt.Errorf("auth: user %q not found", name)
	}
	u.Auth.SessionVersion++
	if u.Auth.SessionVersion < 2 {
		u.Auth.SessionVersion = 2
	}
	u.Auth.Deleted = true
	u.Auth.PasswordHash = ""
	u.Mounts = []Mount{}
	u.Plugins = []string{}
	return Save(dir, u)
}

// Authenticate reports whether name and password match. An unknown user and
// a wrong password both return false, nil.
func Authenticate(dir, name, password string) (bool, error) {
	if !IsValidUsername(name) || password == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false, nil
	}
	u, err := Load(dir, name)
	if err != nil {
		if os.IsNotExist(err) {
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			return false, nil
		}
		return false, err
	}
	if !u.Active() || u.Auth.PasswordHash == "" {
		// Tombstoned or credential-less records still pay the bcrypt cost so
		// timing does not reveal the account state.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.Auth.PasswordHash), []byte(password)) != nil {
		return false, nil
	}
	return true, nil
}

// Load reads <dir>/<name>.yaml.
func Load(dir, name string) (*User, error) {
	if !IsValidUsername(name) {
		return nil, fmt.Errorf("auth: invalid username %q", name)
	}
	path := filepath.Join(dir, name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var u User
	if err := yaml.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	u.Name = name
	if u.Mounts == nil {
		u.Mounts = []Mount{}
	}
	if u.Plugins == nil {
		u.Plugins = []string{}
	}
	return &u, nil
}

// Save writes the user record. The plaintext password is not part of User.
func Save(dir string, u *User) error {
	if u == nil || !IsValidUsername(u.Name) {
		return errors.New("auth: invalid username")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if u.Mounts == nil {
		u.Mounts = []Mount{}
	}
	if u.Plugins == nil {
		u.Plugins = []string{}
	}
	data, err := yaml.Marshal(u)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, u.Name+".yaml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
