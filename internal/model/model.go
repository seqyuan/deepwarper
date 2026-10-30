package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type State string

const (
	StatePending  State = "pending"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

func (s State) Terminal() bool { return s == StateStopped || s == StateFailed }
func (s State) Active() bool {
	return s == StatePending || s == StateStarting || s == StateRunning || s == StateStopping
}

type Instance struct {
	ID           string `yaml:"id"`
	User         string `yaml:"user"`
	Slug         string `yaml:"slug"`
	Distribution string `yaml:"distribution"`
	DistVersion  string `yaml:"distVersion"`
	DistDigest   string `yaml:"distDigest"`
	State        State  `yaml:"state"`
	Backend      string `yaml:"backend"`
	// BackendRef is the systemd unit name in unit mode, empty for a plain process.
	BackendRef  string `yaml:"backendRef,omitempty"`
	Incarnation string `yaml:"incarnation"`
	UnitName    string `yaml:"unitName"`
	Home        string `yaml:"home"`
	Workspace   string `yaml:"workspace"`
	Profile     string `yaml:"profile"`
	PublicHost  string `yaml:"publicHost"`
	TrustedHost string `yaml:"trustedHost"`
	// Port is the loopback port the gateway dials.
	Port     int    `yaml:"port"`
	Endpoint string `yaml:"endpoint"`
	PID      int    `yaml:"pid,omitempty"`
	PIDStart uint64 `yaml:"pidStart,omitempty"`
	// Node is the hostname published in ready.json.
	Node       string   `yaml:"node,omitempty"`
	ControlDir string   `yaml:"controlDir"`
	LogPath    string   `yaml:"logPath"`
	Command    []string `yaml:"command,omitempty"`
	Limiter    string   `yaml:"limiter,omitempty"`
	CPU        int      `yaml:"cpu,omitempty"`
	Memory     string   `yaml:"memory,omitempty"`
	Walltime   string   `yaml:"walltime,omitempty"`
	GPU        int      `yaml:"gpu,omitempty"`
	IdleTTL    string   `yaml:"idleTTL,omitempty"`
	// Revision guards read-modify-write cycles through Store.Update. It is 0
	// for a record that does not exist on disk yet.
	Revision          uint64    `yaml:"revision,omitempty"`
	StopAttempts      int       `yaml:"stopAttempts,omitempty"`
	LastStopAttemptAt time.Time `yaml:"lastStopAttemptAt,omitempty"`
	CreatedAt         time.Time `yaml:"createdAt"`
	StartedAt         time.Time `yaml:"startedAt,omitempty"`
	ReadyAt           time.Time `yaml:"readyAt,omitempty"`
	LastActiveAt      time.Time `yaml:"lastActiveAt,omitempty"`
	EndedAt           time.Time `yaml:"endedAt,omitempty"`
	Error             string    `yaml:"error,omitempty"`
}

type User struct {
	UserID string `yaml:"userId"`
	Slug   string `yaml:"slug"`
	Home   string `yaml:"home"`
}

var (
	slugRE      = regexp.MustCompile(`[^a-z0-9-]+`)
	userIDRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+-]*$`)
	distKeyRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	localSlugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
	}
	if s == "" {
		s = "user"
	}
	return s
}

// ValidateUserID accepts the platform identity that may be stored on an
// instance. It is deliberately narrower than a general path component: no
// separators, no NUL/control bytes, no leading or trailing dot.
func ValidateUserID(s string) error {
	if s == "" {
		return errors.New("user id is empty")
	}
	if len(s) > 128 {
		return fmt.Errorf("user id is longer than 128 bytes")
	}
	if !userIDRE.MatchString(s) {
		return fmt.Errorf("user id %q contains invalid characters", s)
	}
	if strings.HasSuffix(s, ".") {
		return fmt.Errorf("user id %q must not end with a dot", s)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("user id %q is not a valid name", s)
	}
	return nil
}

// ValidateDistKey accepts the keys allowed in distributions: lower-case
// alphanumerics with interior dashes, at most 32 bytes.
func ValidateDistKey(s string) error {
	if !distKeyRE.MatchString(s) {
		return fmt.Errorf("distribution key %q must match [a-z0-9][a-z0-9-]{0,31}", s)
	}
	return nil
}

// IsSafeFileComponent reports whether s can be used as one path element under
// a data directory.
func IsSafeFileComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// StableSlug derives a filesystem-safe slug that cannot accidentally equal a
// canonical local account name for a non-canonical input.
func StableSlug(user string) string {
	base := Slugify(user)
	sum := sha256.Sum256([]byte(user))
	return base + "-" + hex.EncodeToString(sum[:8])
}

// LocalSlug maps an identity to the local account/home name. Canonical auth
// usernames are used verbatim; anything else gets the stable hash suffix so
// "Alice" and "alice" never share a home.
func LocalSlug(user string) string {
	if localSlugRE.MatchString(user) {
		return user
	}
	return StableSlug(user)
}

// InstanceID is the unambiguous, filesystem-safe instance key. The hash of
// (user, dist) keeps ("a", "b-c") and ("a-b", "c") apart.
func InstanceID(user, dist string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + dist))
	return Slugify(user) + "-" + hex.EncodeToString(sum[:8]) + "-" + Slugify(dist)
}

// LegacyInstanceID is the pre-revision "<user>-<dist>" key. It is only used
// to find and preserve records written by older releases.
func LegacyInstanceID(user, dist string) string {
	return user + "-" + dist
}
