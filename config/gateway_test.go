package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestDefaultGatewayConfig_ThreeAgentsEnabled(t *testing.T) {
	c := DefaultGatewayConfig()
	for _, name := range []string{"codex", "claude-code", "kimi"} {
		agent, ok := c.Agents[name]
		if !ok {
			t.Fatalf("default config missing agent %q", name)
		}
		if !agent.Enabled {
			t.Errorf("agent %q must default to enabled", name)
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

func TestGatewayConfig_InvalidPermissionRejected(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Agents["codex"] = AgentConfig{Enabled: true, Permission: "always"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Errorf("invalid permission: want error mentioning permission, got %v", err)
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
	if !c.Agents["claude-code"].Enabled {
		t.Error("claude-code must retain default enabled (map defaults merge)")
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
