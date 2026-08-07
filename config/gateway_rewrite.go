package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Runtime mode profiles for GatewayRuntimeConfig.Mode. Only the "test"
// profile may disable nsjail isolation.
const (
	ModeProd = "prod"
	ModeDev  = "dev"
	ModeTest = "test"
)

// Permission modes for AgentRuntimeConfig.Permission. Permission is part of
// the canonical runtime contract; phase 1 defaults to auto-approving inside
// the controlled workspace, but the mode switch must exist from the start.
const (
	PermissionAuto = "auto"
	PermissionAsk  = "ask"
	PermissionDeny = "deny"
)

// Seccomp policy modes for IsolationConfig.Seccomp.Policy.
const (
	SeccompKafel = "kafel"
	SeccompOff   = "off"
)

// GatewayRuntimeConfig is the NEW agent-gateway configuration. It deliberately
// lives under a distinct name — rather than reusing config.GatewayConfig in
// gateway.go — so it can coexist with the legacy gateway config until the old
// server/cmd/gateway are removed (task 7). The legacy config.GatewayConfig is
// left untouched.
//
// Clients only ever submit an opaque workspace_id; the server resolves it to a
// directory under Workspace.Root. There is deliberately no client-controllable
// workDir anywhere in this shape, so an absolute path can never be injected
// through the public contract.
type GatewayRuntimeConfig struct {
	// Mode is the runtime profile: "prod" (default), "dev", or "test". Only
	// the "test" profile may disable nsjail isolation.
	Mode string `toml:"mode"`

	Server    ServerConfig    `toml:"server"`
	Auth      AuthConfig      `toml:"auth"`
	Workspace WorkspaceConfig `toml:"workspace"`
	Isolation IsolationConfig `toml:"isolation"`

	// Agents configures individual agents, keyed by model/agent ID
	// (e.g. "codex", "claude-code", "kimi").
	Agents map[string]AgentRuntimeConfig `toml:"agents"`
}

// ServerConfig configures the HTTP API listener (routes land in task 5).
type ServerConfig struct {
	// ListenAddr is the address the HTTP API binds to. Default ":4096".
	ListenAddr string `toml:"listen_addr"`
	// ShutdownTimeout bounds graceful shutdown. Default 10s.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`
}

// AuthConfig configures HTTP API authentication.
type AuthConfig struct {
	// Token is the shared bearer token accepted by the API. Empty disables
	// authentication (dev/test only; production deployments should set it).
	Token string `toml:"token"`
}

// WorkspaceConfig configures server-side workspace resolution.
type WorkspaceConfig struct {
	// Root is the directory all workspace_ids resolve under. Only the server
	// owns this path; clients reference workspaces by opaque ID.
	Root string `toml:"root"`
}

// IsolationConfig configures the nsjail sandbox boundary (phase 1). nsjail is
// mandatory in prod and dev; only the "test" runtime mode may set Required to
// false. The nsjail version and source are pinned so deployments are
// reproducible.
type IsolationConfig struct {
	// Required gates whether every worker must run inside nsjail. Default
	// true for prod and dev; Validate rejects Required=false outside the
	// "test" profile.
	Required bool `toml:"required"`

	// NsjailVersion pins the upstream nsjail version (tag or commit).
	NsjailVersion string `toml:"nsjail_version"`
	// NsjailSource pins the upstream source repository/URL.
	NsjailSource string `toml:"nsjail_source"`
	// BinaryPath is the path to the nsjail executable.
	BinaryPath string `toml:"binary_path"`
	// ProfileOverride, when set, replaces the generated nsjail config file.
	ProfileOverride string `toml:"profile_override"`

	// Mounts configures the sandbox mount layout.
	Mounts MountsConfig `toml:"mounts"`
	// Rlimits applies resource limits to jailed processes.
	Rlimits RlimitsConfig `toml:"rlimits"`
	// UserNamespace configures unprivileged user-namespace de-privileging.
	UserNamespace UserNamespaceConfig `toml:"user_namespace"`
	// Seccomp configures the seccomp-bpf policy.
	Seccomp SeccompConfig `toml:"seccomp"`

	// NetworkNamespace creates a dedicated network namespace when true.
	// Default false in phase 1 so agents can reach their providers; egress is
	// controlled by the container/infrastructure instead.
	NetworkNamespace bool `toml:"network_namespace"`
}

// MountsConfig configures where real directories are mounted inside the jail.
// Workspace and agent home are writable; /tmp is a per-session tmpfs.
type MountsConfig struct {
	// WorkspaceDir is the sandbox mount point for the controlled workspace.
	// Default "/workspace".
	WorkspaceDir string `toml:"workspace_dir"`
	// AgentHomeDir is the per-session writable agent home. Default
	// "/agent-home".
	AgentHomeDir string `toml:"agent_home_dir"`
	// TmpDir is the per-session tmpfs mount point. Default "/tmp".
	TmpDir string `toml:"tmp_dir"`
}

// RlimitsConfig configures per-process resource limits inside the jail.
// Zero values mean "no limit set" for size limits and "no cap" for counts
// unless the field is explicitly configured.
type RlimitsConfig struct {
	// MaxOpenFiles caps open file descriptors (RLIMIT_NOFILE).
	MaxOpenFiles int `toml:"max_open_files"`
	// MaxProcesses caps processes/threads (RLIMIT_NPROC).
	MaxProcesses int `toml:"max_processes"`
	// MaxCoreDumpBytes caps core dump size (RLIMIT_CORE). 0 = none.
	MaxCoreDumpBytes int64 `toml:"max_core_dump_bytes"`
	// MaxAddressSpaceBytes caps the address space (RLIMIT_AS). 0 = unlimited.
	MaxAddressSpaceBytes int64 `toml:"max_address_space_bytes"`
}

// UserNamespaceConfig configures uid/gid mapping for unprivileged operation
// (no CAP_SYS_ADMIN, not privileged).
type UserNamespaceConfig struct {
	// Enabled enables user-namespace de-privileging. Default true.
	Enabled bool `toml:"enabled"`
	// UID is the sandbox uid mapped from an unprivileged host uid.
	UID int `toml:"uid"`
	// GID is the sandbox gid mapped from an unprivileged host gid.
	GID int `toml:"gid"`
}

// SeccompConfig configures the seccomp-bpf policy.
type SeccompConfig struct {
	// Policy is "kafel" (default) or "off". "off" is only honored for the
	// "test" runtime mode.
	Policy string `toml:"policy"`
	// ProfileFile, when set, overrides the generated Kafel policy.
	ProfileFile string `toml:"profile_file"`
}

// AgentRuntimeConfig configures one agent adapter. The name of this type (as
// opposed to the legacy config.AgentConfig) avoids colliding with the old
// project/agent config still in use until task 7.
type AgentRuntimeConfig struct {
	// Enabled toggles whether the agent is available. Defaults to enabled for
	// the three first-generation agents.
	Enabled bool `toml:"enabled"`
	// Command is the agent CLI executable. Empty means the adapter resolves
	// its own binary.
	Command string `toml:"command"`
	// DefaultModel is the LLM used when the request does not pin one. Empty
	// means the adapter default.
	DefaultModel string `toml:"default_model"`
	// Permission is "auto", "ask", or "deny". Empty defaults to "auto".
	Permission string `toml:"permission"`
	// Timeout bounds a single turn; zero means the adapter default.
	Timeout time.Duration `toml:"timeout"`
	// MaxConcurrency caps concurrent sessions for this agent; 0 = unlimited.
	MaxConcurrency int `toml:"max_concurrency"`
	// Env adds environment variables for the agent CLI.
	Env map[string]string `toml:"env"`
}

// DefaultGatewayRuntimeConfig returns the recommended defaults. nsjail
// isolation is required for prod and dev; only the test profile may disable
// it. The three first-generation agents default to enabled with auto
// permission.
func DefaultGatewayRuntimeConfig() GatewayRuntimeConfig {
	return GatewayRuntimeConfig{
		Mode: ModeProd,
		Server: ServerConfig{
			ListenAddr:      ":4096",
			ShutdownTimeout: 10 * time.Second,
		},
		Workspace: WorkspaceConfig{
			Root: "workspaces",
		},
		Isolation: IsolationConfig{
			Required:      true,
			NsjailVersion: "0.12.0",
			NsjailSource:  "https://github.com/google/nsjail",
			BinaryPath:    "/usr/local/bin/nsjail",
			Mounts: MountsConfig{
				WorkspaceDir: "/workspace",
				AgentHomeDir: "/agent-home",
				TmpDir:       "/tmp",
			},
			Rlimits: RlimitsConfig{
				MaxOpenFiles: 1024,
				MaxProcesses: 256,
			},
			UserNamespace: UserNamespaceConfig{
				Enabled: true,
				UID:     1000,
				GID:     1000,
			},
			Seccomp: SeccompConfig{
				Policy: SeccompKafel,
			},
			NetworkNamespace: false,
		},
		Agents: map[string]AgentRuntimeConfig{
			"codex":       {Enabled: true, Permission: PermissionAuto},
			"claude-code": {Enabled: true, Permission: PermissionAuto},
			"kimi":        {Enabled: true, Permission: PermissionAuto},
		},
	}
}

// LoadGatewayRuntime reads and validates a gateway TOML config. Keys absent
// from the file retain the defaults returned by DefaultGatewayRuntimeConfig.
func LoadGatewayRuntime(path string) (*GatewayRuntimeConfig, error) {
	cfg := DefaultGatewayRuntimeConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gateway config: %w", err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse gateway config: %w", err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// normalize lower-cases enumerable fields and fills in empty defaults so
// Validate can compare against canonical values.
func (c *GatewayRuntimeConfig) normalize() {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = ModeProd
	}
	c.Isolation.Seccomp.Policy = strings.ToLower(strings.TrimSpace(c.Isolation.Seccomp.Policy))
	if c.Isolation.Seccomp.Policy == "" {
		c.Isolation.Seccomp.Policy = SeccompKafel
	}
	for name, agent := range c.Agents {
		agent.Permission = strings.ToLower(strings.TrimSpace(agent.Permission))
		if agent.Permission == "" {
			agent.Permission = PermissionAuto
		}
		c.Agents[name] = agent
	}
}

// Validate checks the gateway runtime config for consistency.
func (c *GatewayRuntimeConfig) Validate() error {
	switch c.Mode {
	case ModeProd, ModeDev, ModeTest:
	default:
		return fmt.Errorf("config: gateway mode must be one of %q, %q, %q", ModeProd, ModeDev, ModeTest)
	}

	if !c.Isolation.Required && c.Mode != ModeTest {
		return fmt.Errorf("config: isolation.required must be true in mode %q; only the test profile may disable nsjail", c.Mode)
	}
	if c.Isolation.Required {
		if strings.TrimSpace(c.Isolation.NsjailVersion) == "" {
			return fmt.Errorf("config: isolation.nsjail_version must be pinned while isolation is required")
		}
		if strings.TrimSpace(c.Isolation.NsjailSource) == "" {
			return fmt.Errorf("config: isolation.nsjail_source must be pinned while isolation is required")
		}
	}

	switch c.Isolation.Seccomp.Policy {
	case SeccompKafel:
	case SeccompOff:
		if c.Mode != ModeTest {
			return fmt.Errorf("config: isolation.seccomp.policy %q is only allowed in the test profile", SeccompOff)
		}
	default:
		return fmt.Errorf("config: isolation.seccomp.policy %q invalid (want %q or %q)", c.Isolation.Seccomp.Policy, SeccompKafel, SeccompOff)
	}

	if strings.TrimSpace(c.Workspace.Root) == "" {
		return fmt.Errorf("config: workspace.root must not be empty")
	}

	for name, agent := range c.Agents {
		if !agent.Enabled {
			continue
		}
		switch agent.Permission {
		case PermissionAuto, PermissionAsk, PermissionDeny:
		default:
			return fmt.Errorf("config: agents.%s.permission %q invalid (want %q, %q, or %q)",
				name, agent.Permission, PermissionAuto, PermissionAsk, PermissionDeny)
		}
	}
	return nil
}
