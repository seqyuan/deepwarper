package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type State string

const (
	StatePending  State = "pending"
	StateStarting State = "starting"
	// StateQueued is a scheduler-backed instance that is submitted but has no
	// slot yet. It is alive: the job exists and will start by itself.
	StateQueued State = "queued"
	// StateSuspended is SGE's s/S: the job is paused by the scheduler and may
	// resume. It is never reaped and never resubmitted.
	StateSuspended State = "suspended"
	StateRunning   State = "running"
	StateStopping  State = "stopping"
	StateStopped   State = "stopped"
	StateFailed    State = "failed"
)

func (s State) Terminal() bool { return s == StateStopped || s == StateFailed }
func (s State) Active() bool {
	return s == StatePending || s == StateStarting || s == StateQueued ||
		s == StateSuspended || s == StateRunning || s == StateStopping
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
	// BackendRef is the backend's own handle: a systemd unit name for unit
	// mode, an SGE job id for the sge backend, empty for plain process mode.
	// Records written before unit mode also have it empty, which keeps them on
	// the pid path.
	BackendRef  string `yaml:"backendRef,omitempty"`
	Incarnation string `yaml:"incarnation"`
	UnitName    string `yaml:"unitName"`
	Home        string `yaml:"home"`
	Workspace   string `yaml:"workspace"`
	Profile     string `yaml:"profile"`
	PublicHost  string `yaml:"publicHost"`
	TrustedHost string `yaml:"trustedHost"`
	// Port is the loopback port the gateway dials: the local dsh port for the
	// local backend, the login-node end of the ssh tunnel for SGE.
	Port     int    `yaml:"port"`
	Endpoint string `yaml:"endpoint"`
	PID      int    `yaml:"pid,omitempty"`
	PIDStart uint64 `yaml:"pidStart,omitempty"`
	// Node is the compute node the scheduler placed the job on (SGE only).
	Node string `yaml:"node,omitempty"`
	// JobPort is the loopback port `dw instance` picked on the job host (SGE
	// only): the far end of the tunnel, never the port the gateway dials.
	JobPort int `yaml:"jobPort,omitempty"`
	// RemoteEndpoint is "<node>:<jobPort>", the tunnel target (SGE only).
	RemoteEndpoint string `yaml:"remoteEndpoint,omitempty"`
	// LeaseExpiresAt is when the scheduler will end the job (startedAt + h_rt).
	LeaseExpiresAt time.Time `yaml:"leaseExpiresAt,omitempty"`
	// LeaseWarnedAt deduplicates the h_rt warning for one lease window and is
	// re-armed by a successful renewal.
	LeaseWarnedAt time.Time `yaml:"leaseWarnedAt,omitempty"`
	ControlDir    string    `yaml:"controlDir"`
	LogPath       string    `yaml:"logPath"`
	Command       []string  `yaml:"command,omitempty"`
	Limiter       string    `yaml:"limiter,omitempty"`
	CPU           int       `yaml:"cpu,omitempty"`
	Memory        string    `yaml:"memory,omitempty"`
	Walltime      string    `yaml:"walltime,omitempty"`
	GPU           int       `yaml:"gpu,omitempty"`
	IdleTTL       string    `yaml:"idleTTL,omitempty"`
	CreatedAt     time.Time `yaml:"createdAt"`
	StartedAt     time.Time `yaml:"startedAt,omitempty"`
	ReadyAt       time.Time `yaml:"readyAt,omitempty"`
	LastActiveAt  time.Time `yaml:"lastActiveAt,omitempty"`
	EndedAt       time.Time `yaml:"endedAt,omitempty"`
	Error         string    `yaml:"error,omitempty"`
}

type User struct {
	UserID string `yaml:"userId"`
	Slug   string `yaml:"slug"`
	Home   string `yaml:"home"`
}

var slugRE = regexp.MustCompile(`[^a-z0-9-]+`)

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

func StableSlug(user string) string {
	base := Slugify(user)
	sum := sha256.Sum256([]byte(user))
	return base + "-" + hex.EncodeToString(sum[:3])
}

func InstanceID(user, dist string) string {
	return fmt.Sprintf("%s-%s", user, dist)
}
