package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Runtime mode profiles for GatewayConfig.Mode. Only the "test"
// profile may disable nsjail isolation.
const (
	ModeProd = "prod"
	ModeDev  = "dev"
	ModeTest = "test"
)

// Permission modes for AgentConfig.Permission. Permission is part of
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

// GatewayConfig is the agent-gateway configuration. Clients only ever submit
// an opaque workspace_id; the server resolves it to a directory under
// Workspace.Root. There is deliberately no client-controllable workDir anywhere
// in this shape, so an absolute path can never be injected through the public
// contract.
type GatewayConfig struct {
	// Mode is the runtime profile: "prod" (default), "dev", or "test". Only
	// the "test" profile may disable nsjail isolation.
	Mode string `toml:"mode"`

	Server    ServerConfig    `toml:"server"`
	Worker    WorkerConfig    `toml:"worker"`
	Sessions  SessionsConfig  `toml:"sessions"`
	Limits    LimitsConfig    `toml:"limits"`
	Auth      AuthConfig      `toml:"auth"`
	Workspace WorkspaceConfig `toml:"workspace"`
	Isolation IsolationConfig `toml:"isolation"`

	// Agents configures individual agents, keyed by model/agent ID
	// (e.g. "codex", "claude-code", "kimi").
	Agents map[string]AgentConfig `toml:"agents"`
}

// ServerConfig configures the HTTP API listener.
type ServerConfig struct {
	// ListenAddr is the address the HTTP API binds to. Default ":4096".
	ListenAddr string `toml:"listen_addr"`
	// DrainTimeout bounds HTTP request draining during Gateway shutdown.
	DrainTimeout time.Duration `toml:"drain_timeout"`
	// ReadHeaderTimeout bounds how long a request may take to send headers
	// (slow-header/slowloris defense). Must be positive.
	ReadHeaderTimeout time.Duration `toml:"read_header_timeout"`
	// IdleTimeout bounds how long a keep-alive connection may sit idle between
	// requests. Must be positive.
	IdleTimeout time.Duration `toml:"idle_timeout"`
	// MaxHeaderBytes caps the total size of all request headers. 0 means the
	// net/http default (1 MiB).
	MaxHeaderBytes int `toml:"max_header_bytes"`
	// TurnTimeout is the gateway run deadline for a single turn: the bound
	// every agent honors when agents.<id>.timeout does not override it
	// (O-F09b). When it fires, the worker aborts the native turn and the API
	// presents a unified timeout error (stream and non-stream alike). 0
	// disables the deadline for agents without a per-agent timeout. Default
	// 10m. Deliberately independent of the HTTP transport timeouts above —
	// SSE responses must never be cut by a server-wide write deadline.
	TurnTimeout time.Duration `toml:"turn_timeout"`
}

// WorkerConfig controls disposable Worker process lifecycle. These settings
// do not determine how long a Gateway conversation exists.
type WorkerConfig struct {
	// StopGracePeriod bounds CloseSession + SIGTERM before SIGKILL escalation.
	StopGracePeriod time.Duration `toml:"stop_grace_period"`
	// HeartbeatInterval controls runtime Worker health probes. Zero disables
	// heartbeat monitoring.
	HeartbeatInterval time.Duration `toml:"heartbeat_interval"`
	// HeartbeatTimeout bounds one health probe.
	HeartbeatTimeout time.Duration `toml:"heartbeat_timeout"`
	// HeartbeatFailures is the consecutive failure threshold before teardown.
	HeartbeatFailures int `toml:"heartbeat_failures"`
}

// SessionsConfig controls reclamation of idle Worker/Agent processes. The
// Gateway session record and native session ID survive process reclamation.
type SessionsConfig struct {
	// IdleTimeout is how long an idle Worker is retained. Zero disables idle
	// reclamation. Active turns are always exempt.
	IdleTimeout time.Duration `toml:"idle_timeout"`
	// ReapInterval controls how often idle Workers are scanned.
	ReapInterval time.Duration `toml:"reap_interval"`
	// RecordTTL is how long a gateway session record (and its native session
	// ID, used to resume) is retained after the last activity. It MUST exceed
	// IdleTimeout so a record survives worker-process reclamation and can still
	// resume. When IdleTimeout is disabled, RecordTTL becomes the sole session
	// lifetime bound and expired sessions' workers are torn down. Zero disables
	// record expiry (records persist indefinitely — not recommended for
	// long-running deployments). Default 168h.
	RecordTTL time.Duration `toml:"record_ttl"`
	// RunRecordTTL is how long a terminal run record (one Agent turn) is
	// retained before the prune loop deletes it. Zero uses the default
	// (24h). Bounded so long-running gateways do not leak one record per
	// turn (O-F11).
	RunRecordTTL time.Duration `toml:"run_record_ttl"`
}

// LimitsConfig configures admission control and resource caps. All fields
// default to 0 (unlimited); positive values are enforced as hard admission
// limits that reject excess work with HTTP 429 (O-F10).
type LimitsConfig struct {
	// MaxWorkers caps the total number of live worker processes across all
	// sessions and callers. 0 = unlimited.
	MaxWorkers int `toml:"max_workers"`
	// MaxActiveRuns caps the total number of concurrently executing turns
	// across all callers. 0 = unlimited.
	MaxActiveRuns int `toml:"max_active_runs"`
	// MaxSessionsPerCaller caps the number of live sessions records a single
	// caller may own. 0 = unlimited.
	MaxSessionsPerCaller int `toml:"max_sessions_per_caller"`
	// MaxSessions caps the total number of live session records across all
	// callers. 0 = unlimited. Enforced atomically at session creation; excess
	// requests get 429.
	MaxSessions int `toml:"max_sessions"`
	// MaxActiveRunsPerCaller caps the number of concurrently executing turns
	// for a single caller. 0 = unlimited.
	MaxActiveRunsPerCaller int `toml:"max_active_runs_per_caller"`
	// MaxActiveRunsPerWorkspace caps the number of concurrently executing
	// turns targeting the same workspace. 0 = unlimited.
	MaxActiveRunsPerWorkspace int `toml:"max_active_runs_per_workspace"`
	// MaxWorkerLogBytes caps the size of each worker's on-disk log file
	// (worker.log). When the cap is reached, the log is truncated with a
	// marker. 0 = unlimited (64 MiB default in DefaultGatewayConfig).
	MaxWorkerLogBytes int64 `toml:"max_worker_log_bytes"`
	// WorkerLogCheckInterval is how often the worker log cap is enforced.
	// Zero uses the default (5s).
	WorkerLogCheckInterval time.Duration `toml:"worker_log_check_interval"`
}

// AuthConfig configures HTTP API authentication. A token authenticates a
// caller, never an end user. Workspace selection remains an opaque caller
// responsibility and is scoped by the authenticated caller ID.
type AuthConfig struct {
	// Required rejects requests without a configured caller token. It defaults
	// to true in production and may be disabled only for test mode.
	Required bool `toml:"required"`
	// Callers is the configured caller/key allowlist. Tokens are never logged or
	// persisted in session records.
	Callers []CallerConfig `toml:"callers"`
}

// CallerConfig binds one or more rotatable bearer tokens to a caller.
type CallerConfig struct {
	ID     string   `toml:"id"`
	Tokens []string `toml:"tokens"`
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

	// CloneNewPID gives each jail its own PID namespace. Default true: the
	// jailed worker becomes PID 1, so killing the nsjail wrapper reliably kills
	// the entire tree (including an Agent CLI that escaped into its own process
	// group), and a compromised agent can no longer signal the gateway or
	// sibling sessions (their PIDs are invisible from inside the namespace). A
	// namespaced, read-only /proc is mounted with it. Keep a false switch as a
	// fallback for hosts where PID namespaces are unavailable.
	CloneNewPID bool `toml:"clone_newpid"`
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
	// TmpfsSizeMiB bounds the per-session tmpfs mounted at TmpDir. Default
	// 256 (MiB). This is the hard per-session cap on /tmp usage — a session
	// cannot exhaust host /tmp.
	TmpfsSizeMiB int `toml:"tmpfs_size_mib"`
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
	// MaxFileBytes caps the size of any single file a jailed process may
	// write (RLIMIT_FSIZE) — the per-session guard against runaway files
	// inside the writable workspace/agent-home. 0 = unlimited. TOTAL
	// workspace usage remains an infrastructure contract (filesystem quota
	// on the workspace root); the gateway bounds processes, logs, sessions,
	// tmpfs, and per-file sizes.
	MaxFileBytes int64 `toml:"max_file_bytes"`
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
}

// AgentConfig configures one agent adapter.
// Settings are passed as a trusted worker start configuration.
type AgentConfig struct {
	// Enabled toggles whether the agent is available. Defaults to enabled for
	// the three first-generation agents.
	Enabled bool `toml:"enabled"`
	// Command is the agent CLI command in argv form. Empty means the adapter
	// resolves its own binary. The canonical form is an argv array
	// (`command = ["/opt/tools/codex", "--flag"]`); a single string is still
	// accepted for one deprecation window (O-F14).
	Command CommandSpec `toml:"command"`
	// DefaultModel is the LLM used when the request does not pin one. Empty
	// means the adapter default.
	DefaultModel string `toml:"default_model"`
	// Permission is "auto", "ask", or "deny". Empty defaults to "auto".
	Permission string `toml:"permission"`
	// Timeout bounds a single turn for every adapter (the unified turn-deadline
	// contract, O-F09b); zero falls back to server.turn_timeout. When the
	// deadline fires the worker aborts the native turn.
	Timeout time.Duration `toml:"timeout"`
	// MaxConcurrency caps concurrent sessions for this agent; 0 = unlimited.
	MaxConcurrency int `toml:"max_concurrency"`
	// Env adds environment variables for the agent CLI.
	Env    map[string]string `toml:"env"`
	Models []string          `toml:"models"`
	// InjectSystemPrompt forwards the caller-supplied system role messages into
	// the native prompt at each turn. Default false: system messages are
	// ignored so a client cannot pollute the agent's own tool/skill surface.
	InjectSystemPrompt bool `toml:"inject_system_prompt"`
}

// DefaultGatewayConfig returns the recommended defaults. nsjail
// isolation is required for prod and dev; only the test profile may disable
// it. The three first-generation agents default to enabled with auto
// permission.
func DefaultGatewayConfig() GatewayConfig {
	return GatewayConfig{
		Mode: ModeProd,
		Server: ServerConfig{
			ListenAddr:        ":4096",
			DrainTimeout:      30 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    1 << 20, // 1 MiB (net/http default)
			TurnTimeout:       10 * time.Minute,
		},
		Worker: WorkerConfig{
			StopGracePeriod:   10 * time.Second,
			HeartbeatInterval: 15 * time.Second,
			HeartbeatTimeout:  3 * time.Second,
			HeartbeatFailures: 3,
		},
		Sessions: SessionsConfig{
			IdleTimeout:  2 * time.Hour,
			ReapInterval: time.Minute,
			RecordTTL:    168 * time.Hour,
			RunRecordTTL: 24 * time.Hour,
		},
		Limits: LimitsConfig{
			MaxWorkerLogBytes: 64 << 20, // 64 MiB
		},
		Auth: AuthConfig{Required: true},
		Workspace: WorkspaceConfig{
			Root: "workspaces",
		},
		Isolation: IsolationConfig{
			Required:      true,
			NsjailVersion: "3.6",
			NsjailSource:  "https://github.com/google/nsjail",
			BinaryPath:    "/usr/local/bin/nsjail",
			Mounts: MountsConfig{
				WorkspaceDir: "/workspace",
				AgentHomeDir: "/agent-home",
				TmpDir:       "/tmp",
				TmpfsSizeMiB: 256,
			},
			Rlimits: RlimitsConfig{
				MaxOpenFiles: 1024,
				MaxProcesses: 256,
			},
			UserNamespace: UserNamespaceConfig{
				Enabled: true,
				// 65532 matches the container entrypoint (docker/entrypoint.sh)
				// and the non-privileged smoke gate. 1000 (an arbitrary host uid)
				// is a deployment foot-gun: an unprivileged process can only map
				// its own host uid, so a 1000:1000 default fails preflight when the
				// container runs as 65532 (runAsUser/Group in the documented k8s
				// securityContext).
				UID: 65532,
				GID: 65532,
			},
			Seccomp: SeccompConfig{
				Policy: SeccompKafel,
			},
			NetworkNamespace: false,
			CloneNewPID:      true,
		},
		// Agents default to disabled: the base image ships no agent CLIs, so
		// /v1/models must not advertise a CLI that is not installed. An
		// operator enables an agent AND sets agents.<id>.command (with the
		// CLI installed in the image) to make it available; model discovery
		// then probes that command before advertising it (O-F04).
		Agents: map[string]AgentConfig{
			"codex":       {Enabled: false, Permission: PermissionAuto},
			"claude-code": {Enabled: false, Permission: PermissionAuto},
			"kimi":        {Enabled: false, Permission: PermissionAuto},
		},
	}
}

// LoadGateway reads and validates a gateway TOML config. Keys absent
// from the file retain the defaults returned by DefaultGatewayConfig.
func LoadGateway(path string) (*GatewayConfig, error) {
	cfg := DefaultGatewayConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gateway config: %w", err)
	}
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse gateway config: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		return nil, fmt.Errorf("parse gateway config: unknown keys: %s", strings.Join(keys, ", "))
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	for name, agent := range cfg.Agents {
		if agent.Command.IsLegacyString() {
			warnLegacyCommand(name, agent.Command)
		}
	}
	return &cfg, nil
}

// normalize lower-cases enumerable fields and fills in empty defaults so
// Validate can compare against canonical values.
func (c *GatewayConfig) normalize() {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = ModeProd
	}
	c.Isolation.Seccomp.Policy = strings.ToLower(strings.TrimSpace(c.Isolation.Seccomp.Policy))
	if c.Isolation.Seccomp.Policy == "" {
		c.Isolation.Seccomp.Policy = SeccompKafel
	}
	for name, agent := range c.Agents {
		seen := make(map[string]bool)
		models := make([]string, 0, len(agent.Models))
		for _, model := range agent.Models {
			model = strings.TrimSpace(model)
			if model != "" && !seen[model] {
				seen[model] = true
				models = append(models, model)
			}
		}
		agent.Models = models
		agent.Permission = strings.ToLower(strings.TrimSpace(agent.Permission))
		if agent.Permission == "" {
			agent.Permission = PermissionAuto
		}
		c.Agents[name] = agent
	}
	for i := range c.Auth.Callers {
		caller := &c.Auth.Callers[i]
		caller.ID = strings.TrimSpace(caller.ID)
		for j := range caller.Tokens {
			caller.Tokens[j] = strings.TrimSpace(caller.Tokens[j])
		}
	}
}

// Validate checks the gateway runtime config for consistency.
func (c *GatewayConfig) Validate() error {
	switch c.Mode {
	case ModeProd, ModeDev, ModeTest:
	default:
		return fmt.Errorf("config: gateway mode must be one of %q, %q, %q", ModeProd, ModeDev, ModeTest)
	}
	if !c.Isolation.Required && c.Mode != ModeTest {
		return fmt.Errorf("config: isolation.required must be true in mode %q; only the test profile may disable nsjail", c.Mode)
	}
	if c.Server.DrainTimeout <= 0 {
		return fmt.Errorf("config: server.drain_timeout must be positive")
	}
	if c.Server.ReadHeaderTimeout <= 0 {
		return fmt.Errorf("config: server.read_header_timeout must be positive")
	}
	if c.Server.IdleTimeout <= 0 {
		return fmt.Errorf("config: server.idle_timeout must be positive")
	}
	if c.Server.MaxHeaderBytes < 0 {
		return fmt.Errorf("config: server.max_header_bytes must not be negative")
	}
	if c.Server.TurnTimeout < 0 {
		return fmt.Errorf("config: server.turn_timeout must not be negative")
	}
	if c.Worker.StopGracePeriod <= 0 {
		return fmt.Errorf("config: worker.stop_grace_period must be positive")
	}
	if c.Worker.HeartbeatInterval < 0 || c.Worker.HeartbeatTimeout < 0 || c.Worker.HeartbeatFailures < 0 {
		return fmt.Errorf("config: worker heartbeat settings must not be negative")
	}
	if c.Worker.HeartbeatInterval > 0 && (c.Worker.HeartbeatTimeout <= 0 || c.Worker.HeartbeatFailures <= 0) {
		return fmt.Errorf("config: worker heartbeat_timeout and heartbeat_failures must be positive when heartbeat is enabled")
	}
	if c.Sessions.IdleTimeout < 0 || c.Sessions.ReapInterval < 0 {
		return fmt.Errorf("config: session lifecycle settings must not be negative")
	}
	if c.Sessions.IdleTimeout > 0 && c.Sessions.ReapInterval <= 0 {
		return fmt.Errorf("config: sessions.reap_interval must be positive when idle reclamation is enabled")
	}
	if c.Sessions.RecordTTL < 0 {
		return fmt.Errorf("config: sessions.record_ttl must not be negative")
	}
	if c.Sessions.RunRecordTTL < 0 {
		return fmt.Errorf("config: sessions.run_record_ttl must not be negative")
	}
	if c.Sessions.RecordTTL > 0 && c.Sessions.IdleTimeout > 0 && c.Sessions.RecordTTL <= c.Sessions.IdleTimeout {
		return fmt.Errorf("config: sessions.record_ttl must exceed idle_timeout so records survive worker reclamation")
	}
	if c.Isolation.Mounts.TmpfsSizeMiB < 0 {
		return fmt.Errorf("config: isolation.mounts.tmpfs_size_mib must not be negative")
	}
	if c.Limits.MaxWorkers < 0 || c.Limits.MaxActiveRuns < 0 ||
		c.Limits.MaxSessionsPerCaller < 0 || c.Limits.MaxSessions < 0 ||
		c.Limits.MaxActiveRunsPerCaller < 0 ||
		c.Limits.MaxActiveRunsPerWorkspace < 0 || c.Limits.MaxWorkerLogBytes < 0 {
		return fmt.Errorf("config: limits values must not be negative")
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

	// The per-jail PID namespace is a security boundary: it makes a killed
	// nsjail wrapper reap the whole tree and hides sibling sessions'/gateway
	// PIDs from a compromised agent. It must not be silently disabled by an
	// ordinary config outside the test profile (code-review follow-up F6).
	if !c.Isolation.CloneNewPID && c.Mode != ModeTest {
		return fmt.Errorf("config: isolation.clone_newpid=false is only allowed in the test profile; the per-jail PID namespace is a security boundary")
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
		if agent.Timeout < 0 {
			return fmt.Errorf("config: agents.%s.timeout must not be negative", name)
		}
	}
	if c.Mode == ModeProd {
		if !c.Auth.Required {
			return fmt.Errorf("config: auth.required must be true in production")
		}
		if len(c.Auth.Callers) == 0 {
			return fmt.Errorf("config: auth.callers must contain at least one caller in production")
		}
	}
	callerIDs := make(map[string]struct{}, len(c.Auth.Callers))
	tokens := make(map[string]string)
	for _, caller := range c.Auth.Callers {
		if caller.ID == "" {
			return fmt.Errorf("config: auth.callers contains an empty id")
		}
		if _, ok := callerIDs[caller.ID]; ok {
			return fmt.Errorf("config: auth.callers duplicate id %q", caller.ID)
		}
		callerIDs[caller.ID] = struct{}{}
		if len(caller.Tokens) == 0 {
			return fmt.Errorf("config: auth.callers[%q] must contain at least one token", caller.ID)
		}
		for _, token := range caller.Tokens {
			if token == "" {
				return fmt.Errorf("config: auth.callers[%q] contains an empty token", caller.ID)
			}
			if previous, ok := tokens[token]; ok {
				return fmt.Errorf("config: auth.callers duplicate token shared by %q and %q", previous, caller.ID)
			}
			tokens[token] = caller.ID
		}
	}
	return nil
}
