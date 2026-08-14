package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultGatewayConfig_LifecycleGovernance(t *testing.T) {
	c := DefaultGatewayConfig()

	if c.Server.DrainTimeout != 30*time.Second {
		t.Errorf("Server.DrainTimeout = %v, want 30s", c.Server.DrainTimeout)
	}
	if c.Server.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("Server.ReadHeaderTimeout = %v, want 10s", c.Server.ReadHeaderTimeout)
	}
	if c.Server.IdleTimeout != 60*time.Second {
		t.Errorf("Server.IdleTimeout = %v, want 60s", c.Server.IdleTimeout)
	}
	if c.Server.MaxHeaderBytes != 1<<20 {
		t.Errorf("Server.MaxHeaderBytes = %d, want 1 MiB", c.Server.MaxHeaderBytes)
	}
	if c.Worker.StopGracePeriod != 10*time.Second {
		t.Errorf("Worker.StopGracePeriod = %v, want 10s", c.Worker.StopGracePeriod)
	}
	if c.Worker.HeartbeatInterval != 15*time.Second || c.Worker.HeartbeatTimeout != 3*time.Second || c.Worker.HeartbeatFailures != 3 {
		t.Errorf("Worker heartbeat defaults = %v/%v/%d, want 15s/3s/3", c.Worker.HeartbeatInterval, c.Worker.HeartbeatTimeout, c.Worker.HeartbeatFailures)
	}
	if c.Sessions.IdleTimeout != 2*time.Hour || c.Sessions.ReapInterval != time.Minute {
		t.Errorf("Session lifecycle defaults = %v/%v, want 2h/1m", c.Sessions.IdleTimeout, c.Sessions.ReapInterval)
	}
	if c.Sessions.RecordTTL != 168*time.Hour {
		t.Errorf("Session RecordTTL default = %v, want 168h", c.Sessions.RecordTTL)
	}
}

// TestGatewayConfig_RecordTTLMustExceedIdleTimeout is a regression test for the
// session-record leak fix: record_ttl must outlive idle_timeout so a record
// survives worker reclamation and can still resume. A too-short TTL silently
// reaped records while their workers still ran.
func TestGatewayConfig_RecordTTLMustExceedIdleTimeout(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Mode = "test"
	// Equal is rejected; it must strictly exceed.
	c.Sessions.IdleTimeout = 2 * time.Hour
	c.Sessions.RecordTTL = 2 * time.Hour
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate accepted record_ttl == idle_timeout")
	}
	// Longer is accepted.
	c.Sessions.RecordTTL = 3 * time.Hour
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected record_ttl > idle_timeout: %v", err)
	}
	// Negative is rejected.
	c.Sessions.RecordTTL = -time.Second
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate accepted negative record_ttl")
	}
}

func TestLoadGateway_LifecycleGovernance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.toml")
	content := `
mode = "test"

[server]
drain_timeout = "41s"

[worker]
stop_grace_period = "7s"
heartbeat_interval = "9s"
heartbeat_timeout = "2s"
heartbeat_failures = 4

[sessions]
idle_timeout = "45m"
reap_interval = "20s"

[auth]
required = false

[workspace]
root = "workspaces"

[isolation]
required = false

[isolation.seccomp]
policy = "off"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadGateway(path)
	if err != nil {
		t.Fatalf("LoadGateway: %v", err)
	}
	if c.Server.DrainTimeout != 41*time.Second || c.Worker.StopGracePeriod != 7*time.Second {
		t.Fatalf("shutdown settings = %v/%v, want 41s/7s", c.Server.DrainTimeout, c.Worker.StopGracePeriod)
	}
	if c.Worker.HeartbeatInterval != 9*time.Second || c.Worker.HeartbeatTimeout != 2*time.Second || c.Worker.HeartbeatFailures != 4 {
		t.Fatalf("heartbeat settings = %v/%v/%d", c.Worker.HeartbeatInterval, c.Worker.HeartbeatTimeout, c.Worker.HeartbeatFailures)
	}
	if c.Sessions.IdleTimeout != 45*time.Minute || c.Sessions.ReapInterval != 20*time.Second {
		t.Fatalf("session settings = %v/%v", c.Sessions.IdleTimeout, c.Sessions.ReapInterval)
	}
}

func TestLoadGatewayRejectsRemovedShutdownTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.toml")
	content := `
mode = "test"
[server]
shutdown_timeout = "10s"
[auth]
required = false
[workspace]
root = "workspaces"
[isolation]
required = false
[isolation.seccomp]
policy = "off"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGateway(path)
	if err == nil || !strings.Contains(err.Error(), "shutdown_timeout") {
		t.Fatalf("LoadGateway error = %v, want removed shutdown_timeout rejection", err)
	}
}

func TestDefaultGatewayConfig_IsolationRequired(t *testing.T) {
	c := DefaultGatewayConfig()

	if c.Mode != ModeProd {
		t.Errorf("Mode default = %q, want %q", c.Mode, ModeProd)
	}
	if !c.Isolation.Required {
		t.Error("Isolation.Required must default to true (nsjail mandatory in prod/dev)")
	}
	if c.Isolation.NetworkNamespace {
		t.Error("Isolation.NetworkNamespace must default to false in phase 1 (agents must reach providers)")
	}
	if !c.Isolation.CloneNewPID {
		t.Error("Isolation.CloneNewPID must default to true (per-jail PID namespace)")
	}
	if c.Isolation.NsjailVersion == "" {
		t.Error("Isolation.NsjailVersion must be pinned")
	}
	if c.Isolation.NsjailSource == "" {
		t.Error("Isolation.NsjailSource must be pinned")
	}
	if c.Isolation.BinaryPath == "" {
		t.Error("Isolation.BinaryPath must have a default")
	}
	if c.Isolation.Mounts.WorkspaceDir != "/workspace" {
		t.Errorf("Mounts.WorkspaceDir = %q, want %q", c.Isolation.Mounts.WorkspaceDir, "/workspace")
	}
	if c.Isolation.Mounts.AgentHomeDir != "/agent-home" {
		t.Errorf("Mounts.AgentHomeDir = %q, want %q", c.Isolation.Mounts.AgentHomeDir, "/agent-home")
	}
	if !c.Isolation.UserNamespace.Enabled {
		t.Error("UserNamespace.Enabled must default to true")
	}
	if c.Isolation.Seccomp.Policy != SeccompKafel {
		t.Errorf("Seccomp.Policy = %q, want %q", c.Isolation.Seccomp.Policy, SeccompKafel)
	}
}

// TestDefaultGatewayConfig_UserNamespaceUIDMatchesContainer is a regression
// test for the uidmap deployment foot-gun (O-C3): the default uid/gid was 1000,
// but the container entrypoint and non-privileged smoke run as 65532. An
// unprivileged process can only map its own host uid, so a 1000 default made
// nsjail's user-namespace mapping fail preflight in the documented deployment.
// The default must match the container runtime user.
func TestDefaultGatewayConfig_UserNamespaceUIDMatchesContainer(t *testing.T) {
	c := DefaultGatewayConfig()
	if c.Isolation.UserNamespace.UID != 65532 || c.Isolation.UserNamespace.GID != 65532 {
		t.Fatalf("UserNamespace UID/GID = %d/%d, want 65532/65532 (matches docker/entrypoint.sh and the non-privileged smoke gate)", c.Isolation.UserNamespace.UID, c.Isolation.UserNamespace.GID)
	}
}

func TestGatewayConfigRejectsProdWithoutCallers(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Auth.Callers = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "auth.callers") {
		t.Fatalf("Validate() = %v, want auth.callers error", err)
	}
}

func TestGatewayConfigRejectsDuplicateCallerToken(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Auth.Callers = []CallerConfig{
		{ID: "a", Tokens: []string{"same"}},
		{ID: "b", Tokens: []string{"same"}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate token") {
		t.Fatalf("Validate() = %v, want duplicate token error", err)
	}
}

func TestDefaultGatewayConfig_AgentsDisabledByDefault(t *testing.T) {
	c := DefaultGatewayConfig()
	for _, name := range []string{"codex", "claude-code", "kimi"} {
		agent, ok := c.Agents[name]
		if !ok {
			t.Fatalf("default config missing agent %q", name)
		}
		// The base image ships no agent CLIs, so agents default to disabled and
		// /v1/models advertises nothing until an operator enables one AND sets
		// its command (O-F04). This is the regression guard for the
		// "advertise an unusable Agent" finding.
		if agent.Enabled {
			t.Errorf("agent %q must default to disabled (base image has no CLI)", name)
		}
		if agent.Permission != PermissionAuto {
			t.Errorf("agent %q permission = %q, want %q", name, agent.Permission, PermissionAuto)
		}
	}
}

func TestGatewayConfig_OnlyTestMayDisableIsolation(t *testing.T) {
	for _, mode := range []string{ModeProd, ModeDev} {
		c := DefaultGatewayConfig()
		c.Mode = mode
		c.Isolation.Required = false
		if err := c.Validate(); err == nil {
			t.Errorf("mode %q with isolation.required=false: expected validation error", mode)
		}
	}

	c := DefaultGatewayConfig()
	c.Mode = ModeTest
	c.Isolation.Required = false
	if err := c.Validate(); err != nil {
		t.Errorf("test profile may disable isolation, got error: %v", err)
	}
}

func TestGatewayConfig_RequiredIsolationMustPinNsjail(t *testing.T) {
	// Empty pinned version/source while isolation is required must fail.
	c := DefaultGatewayConfig()
	c.Isolation.NsjailVersion = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "nsjail_version") {
		t.Errorf("empty nsjail_version while required: want error mentioning nsjail_version, got %v", err)
	}

	c = DefaultGatewayConfig()
	c.Isolation.NsjailSource = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "nsjail_source") {
		t.Errorf("empty nsjail_source while required: want error mentioning nsjail_source, got %v", err)
	}
}

func TestGatewayConfig_SeccompOffRequiresTestProfile(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Mode = ModeDev
	c.Isolation.Seccomp.Policy = SeccompOff
	if err := c.Validate(); err == nil {
		t.Error("seccomp off in dev: expected validation error")
	}

	c = DefaultGatewayConfig()
	c.Mode = ModeTest
	c.Isolation.Seccomp.Policy = SeccompOff
	if err := c.Validate(); err != nil {
		t.Errorf("seccomp off in test profile: expected no error, got %v", err)
	}

	c = DefaultGatewayConfig()
	c.Isolation.Seccomp.Policy = "kalfel"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "seccomp") {
		t.Errorf("invalid seccomp policy: want error mentioning seccomp, got %v", err)
	}
}

// TestGatewayConfig_CloneNewPIDRequiredOutsideTestProfile is a regression test
// for the PID-namespace config hole (code-review F6): the per-jail PID
// namespace is a security boundary, so an ordinary config must not silently
// disable it outside the test profile.
func TestGatewayConfig_CloneNewPIDRequiredOutsideTestProfile(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Mode = ModeProd
	c.Isolation.CloneNewPID = false
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "clone_newpid") {
		t.Errorf("prod clone_newpid=false: want error mentioning clone_newpid, got %v", err)
	}

	c = DefaultGatewayConfig()
	c.Mode = ModeTest
	c.Isolation.CloneNewPID = false
	if err := c.Validate(); err != nil {
		t.Errorf("test profile may disable clone_newpid, got %v", err)
	}
}

func TestGatewayConfig_InvalidPermissionRejected(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Agents["codex"] = AgentConfig{Enabled: true, Permission: "always"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Errorf("invalid permission: want error mentioning permission, got %v", err)
	}
}

// TestGatewayConfig_PerAgentTimeoutFailClosed is a regression test for the
// silent-misconfiguration bug (O-F09a): agents.<id>.timeout was consumed only
// by the kimi adapter and silently ignored by the others, so an operator could
// configure a turn bound that was then dropped. A non-kimi agent with a
// timeout must now fail closed at startup; kimi is accepted.
func TestGatewayConfig_PerAgentTimeoutFailClosed(t *testing.T) {
	base := func() GatewayConfig {
		c := DefaultGatewayConfig()
		c.Mode = ModeTest // bypass the prod auth-caller requirement
		return c
	}
	// codex with a timeout is rejected.
	c := base()
	c.Agents["codex"] = AgentConfig{Enabled: true, Permission: PermissionAuto, Timeout: 60 * time.Second}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "codex.timeout") {
		t.Errorf("codex timeout: want error mentioning codex.timeout, got %v", err)
	}
	// claude-code with a timeout is rejected.
	c = base()
	c.Agents["claude-code"] = AgentConfig{Enabled: true, Permission: PermissionAuto, Timeout: 60 * time.Second}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "claude-code.timeout") {
		t.Errorf("claude-code timeout: want error mentioning claude-code.timeout, got %v", err)
	}
	// kimi with a timeout is accepted (the only adapter that honors it today).
	c = base()
	c.Agents["kimi"] = AgentConfig{Enabled: true, Permission: PermissionAuto, Timeout: 60 * time.Second}
	if err := c.Validate(); err != nil {
		t.Errorf("kimi timeout: expected no error, got %v", err)
	}
	// A disabled agent with a timeout does not trigger the check.
	c = base()
	c.Agents["codex"] = AgentConfig{Enabled: false, Timeout: 60 * time.Second}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled codex timeout: expected no error, got %v", err)
	}
}

func TestGatewayConfig_InvalidModeRejected(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Mode = "staging"
	if err := c.Validate(); err == nil {
		t.Error("invalid mode: expected validation error")
	}
}

func TestLoadGateway_MergesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.toml")
	content := `
mode = "dev"

[workspace]
root = "/srv/gw-workspaces"

[agents.codex]
enabled = false
permission = "ask"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := LoadGateway(path)
	if err != nil {
		t.Fatalf("LoadGateway: %v", err)
	}

	// Explicit values from the file win.
	if c.Mode != ModeDev {
		t.Errorf("Mode = %q, want %q", c.Mode, ModeDev)
	}
	if c.Workspace.Root != "/srv/gw-workspaces" {
		t.Errorf("Workspace.Root = %q, want %q", c.Workspace.Root, "/srv/gw-workspaces")
	}
	if c.Agents["codex"].Enabled {
		t.Error("codex must be disabled per config")
	}
	if c.Agents["codex"].Permission != PermissionAsk {
		t.Errorf("codex permission = %q, want %q", c.Agents["codex"].Permission, PermissionAsk)
	}

	// Defaults are retained for keys absent from the file.
	if !c.Isolation.Required {
		t.Error("Isolation.Required must retain default true in dev")
	}
	if c.Server.ListenAddr != ":4096" {
		t.Errorf("Server.ListenAddr = %q, want default %q", c.Server.ListenAddr, ":4096")
	}
	if c.Agents["claude-code"].Enabled {
		t.Error("claude-code must retain default disabled (map defaults merge)")
	}
	if c.Agents["kimi"].Permission != PermissionAuto {
		t.Errorf("kimi permission = %q, want default %q", c.Agents["kimi"].Permission, PermissionAuto)
	}
}

func TestLoadGateway_MissingFile(t *testing.T) {
	if _, err := LoadGateway(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadGateway_InjectSystemPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.toml")
	content := `
mode = "dev"

[workspace]
root = "workspaces"

[agents.codex]
inject_system_prompt = true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadGateway(path)
	if err != nil {
		t.Fatalf("LoadGateway: %v", err)
	}
	if !c.Agents["codex"].InjectSystemPrompt {
		t.Error("codex.inject_system_prompt must parse true")
	}
	if c.Agents["claude-code"].InjectSystemPrompt {
		t.Error("claude-code.inject_system_prompt must default false")
	}
}
