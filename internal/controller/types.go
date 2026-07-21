package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	managedLabel  = "io.github.ycxom.docker-control.managed"
	keyLabel      = "io.github.ycxom.docker-control.key"
	endpointLabel = "io.github.ycxom.docker-control.controlled-endpoint"
	healthLabel   = "io.github.ycxom.docker-control.health-endpoint"
	tokenLabel    = "io.github.ycxom.docker-control.token-sha256"
	imageLabel    = "io.github.ycxom.docker-control.image"

	snapshotLabel       = "io.github.ycxom.docker-control.snapshot"
	snapshotKeyLabel    = "io.github.ycxom.docker-control.snapshot-key"
	snapshotIDLabel     = "io.github.ycxom.docker-control.snapshot-id"
	snapshotNameLabel   = "io.github.ycxom.docker-control.snapshot-name"
	snapshotSourceLabel = "io.github.ycxom.docker-control.snapshot-source-image"
	maxSnapshotsPerKey  = 10

	legacyManagedLabel  = "astrbot.plugin.docker_sandbox.managed"
	legacyKeyLabel      = "astrbot.plugin.docker_sandbox.session"
	legacyEndpointLabel = "astrbot.plugin.docker_sandbox.controlled_endpoint"
	legacyTokenLabel    = "astrbot.plugin.docker_sandbox.controlled_token_sha256"
)

type Config struct {
	ListenAddress       string
	DockerSocket        string
	BearerToken         string
	Image               string
	PullMissingImage    bool
	AllowNetwork        bool
	SandboxNetwork      string
	PublicEndpoint      string
	MemoryBytes         int64
	NanoCPUs            int64
	PIDsLimit           int64
	WorkspaceBytes      int64
	ExecutionTimeout    time.Duration
	MaxOutputBytes      int
	MaxFileBytes        int64
	MaxContainers       int
	TerminalIdleTimeout time.Duration
	MaxTerminalWindows  int
	PortFallbacks       int
	RuntimeFile         string
	ContainerNamePrefix string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		ListenAddress:       envAliases(":16544", "DOCKER_CONTROL_LISTEN", "CONTROLLER_LISTEN"),
		DockerSocket:        envAliases("/var/run/docker.sock", "DOCKER_CONTROL_SOCKET", "DOCKER_SOCKET"),
		BearerToken:         envAliases("", "DOCKER_CONTROL_TOKEN", "CONTROLLER_TOKEN"),
		Image:               envAliases("ubuntu:22.04", "DOCKER_CONTROL_IMAGE", "SANDBOX_IMAGE"),
		PullMissingImage:    envBool("PULL_MISSING_IMAGE", true),
		AllowNetwork:        envBool("ALLOW_NETWORK", true),
		SandboxNetwork:      env("SANDBOX_NETWORK", "bridge"),
		PublicEndpoint:      envAliases("auto", "DOCKER_CONTROL_PUBLIC_ENDPOINT", "CONTROLLED_DOCKER_ENDPOINT"),
		MemoryBytes:         envInt64("MEMORY_BYTES", 512*1024*1024),
		NanoCPUs:            envInt64("NANO_CPUS", 1_000_000_000),
		PIDsLimit:           envInt64("PIDS_LIMIT", 128),
		WorkspaceBytes:      envInt64("WORKSPACE_BYTES", 256*1024*1024),
		ExecutionTimeout:    time.Duration(envInt64("EXECUTION_TIMEOUT_SECONDS", 120)) * time.Second,
		MaxOutputBytes:      int(envInt64("MAX_OUTPUT_BYTES", 20_000)),
		MaxFileBytes:        envInt64("MAX_FILE_BYTES", 1024*1024),
		MaxContainers:       int(envInt64("MAX_CONTAINERS", 20)),
		TerminalIdleTimeout: time.Duration(envInt64("TERMINAL_IDLE_SECONDS", 300)) * time.Second,
		MaxTerminalWindows:  int(envInt64("MAX_TERMINAL_WINDOWS", 40)),
		PortFallbacks:       int(envInt64("PORT_FALLBACKS", 20)),
		RuntimeFile:         envAliases("", "DOCKER_CONTROL_RUNTIME_FILE", "CONTROLLER_RUNTIME_FILE"),
		ContainerNamePrefix: env("CONTAINER_NAME_PREFIX", "sandbox"),
	}
	return cfg, cfg.Validate()
}

func (cfg Config) Validate() error {
	if cfg.BearerToken == "" {
		return errors.New("DOCKER_CONTROL_TOKEN or --token is required")
	}
	if cfg.MemoryBytes <= 0 || cfg.NanoCPUs <= 0 || cfg.PIDsLimit <= 0 || cfg.WorkspaceBytes <= 0 || cfg.MaxContainers <= 0 || cfg.TerminalIdleTimeout <= 0 || cfg.MaxTerminalWindows <= 0 {
		return errors.New("resource limits, container limit, terminal TTL and window limit must be positive")
	}
	if cfg.PortFallbacks < 0 {
		return errors.New("port fallbacks cannot be negative")
	}
	if cfg.PublicEndpoint != "auto" && !strings.HasPrefix(cfg.PublicEndpoint, "ws://") && !strings.HasPrefix(cfg.PublicEndpoint, "wss://") {
		return errors.New("DOCKER_CONTROL_PUBLIC_ENDPOINT must use ws://, wss://, or auto")
	}
	if !validNamePrefix(cfg.ContainerNamePrefix) {
		return errors.New("CONTAINER_NAME_PREFIX must contain only lowercase letters, numbers, dots, dashes, or underscores")
	}
	return nil
}

func envAliases(fallback string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return fallback
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

type Container struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Key                string    `json:"key"`
	Image              string    `json:"image"`
	State              string    `json:"state"`
	Ready              bool      `json:"ready"`
	Readiness          string    `json:"readiness"`
	ControlledEndpoint string    `json:"controlled_endpoint"`
	CreatedAt          time.Time `json:"created_at,omitempty"`
}

type CreateRequest struct {
	Key   string `json:"key"`
	Image string `json:"image,omitempty"`
}

type SnapshotRequest struct {
	Name string `json:"name,omitempty"`
}

type Snapshot struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Image       string    `json:"image"`
	SourceImage string    `json:"source_image"`
	CreatedAt   time.Time `json:"created_at"`
	SizeBytes   int64     `json:"size_bytes"`
}

type ExecRequest struct {
	Command []string `json:"command"`
	WorkDir string   `json:"workdir,omitempty"`
}

type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	TimedOut  bool   `json:"timed_out"`
}

type CreateSpec struct {
	Key                      string
	Name                     string
	Image                    string
	NetworkMode              string
	MemoryBytes              int64
	NanoCPUs                 int64
	PIDsLimit                int64
	WorkspaceBytes           int64
	ControlledEndpoint       string
	ControlledHealthEndpoint string
	ControlledToken          string
	ControlledTokenSHA256    string
}

type Engine interface {
	Version(context.Context) (string, error)
	List(context.Context) ([]Container, error)
	Get(context.Context, string) (Container, error)
	Ensure(context.Context, CreateSpec, bool, int) (Container, bool, error)
	Remove(context.Context, string) (bool, error)
	Exec(context.Context, string, ExecRequest, time.Duration, int) (ExecResult, error)
	ExecStream(context.Context, string, ExecRequest, time.Duration, int, func([]byte) error) (ExecResult, error)
	WriteFile(context.Context, string, string, []byte) error
	ReadFile(context.Context, string, string, int64) ([]byte, bool, error)
}

type SnapshotEngine interface {
	CreateSnapshot(context.Context, string, string, int) (Snapshot, error)
	ListSnapshots(context.Context, string) ([]Snapshot, error)
	RestoreSnapshot(context.Context, string, string, CreateSpec) (Container, error)
	RemoveSnapshot(context.Context, string, string) (bool, error)
}

var ErrNotFound = errors.New("not found")
var ErrImagePreparing = errors.New("sandbox image is being prepared")
var ErrSandboxNotReady = errors.New("sandbox is not ready")
var ErrInvalidSandboxKey = errors.New("invalid sandbox key")
var ErrSnapshotLimit = errors.New("snapshot limit reached")

func validKey(value string) bool {
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func validImage(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 255 && !strings.ContainsAny(value, "\r\n\x00")
}

func validSnapshotID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'f') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

func validSnapshotName(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) <= 64 && !strings.ContainsAny(value, "\r\n\x00")
}

func validNamePrefix(value string) bool {
	if value == "" || len(value) > 40 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

func controlledTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func secureEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func safeWorkspacePath(path string) (string, error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	if cleaned == "" || strings.HasPrefix(cleaned, "/") {
		return "", errors.New("path must be relative to /workspace")
	}
	parts := strings.Split(cleaned, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("path must stay inside /workspace")
		}
	}
	return "/workspace/" + strings.Join(parts, "/"), nil
}

func endpointFor(base, key string) string {
	return fmt.Sprintf("%s/v1/controlled/%s/terminal", strings.TrimRight(base, "/"), key)
}

func healthEndpointFor(base, key string) string {
	httpBase := strings.TrimRight(base, "/")
	httpBase = strings.TrimPrefix(httpBase, "ws://")
	httpBase = strings.TrimPrefix(httpBase, "wss://")
	scheme := "http://"
	if strings.HasPrefix(base, "wss://") {
		scheme = "https://"
	}
	return fmt.Sprintf("%s%s/v1/controlled/%s", scheme, httpBase, key)
}
