//go:build agent_ref

package codex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// Codex stores MCP servers in $CODEX_HOME/config.toml under [mcp_servers.<name>].
// Each stdio entry looks like:
//
//   [mcp_servers.filesystem]
//   command = "npx"
//   args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
//
//   [mcp_servers.filesystem.env]
//   KEY = "value"
//
// SSE/HTTP entries use `url` instead. All editing goes through BurntSushi
// decode → generic map → re-encode, which preserves unknown sections but, like
// the provider config, loses comments (accepted per plan).

// listCodexMcpServers reads [mcp_servers.*] from config.toml.
func listCodexMcpServers(codexHome string) (map[string]core.McpServerConfig, error) {
	home, err := resolveCodexHomeForConfig(codexHome)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]core.McpServerConfig{}, nil
		}
		return nil, err
	}

	var doc struct {
		McpServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("codex: parse config.toml: %w", err)
	}

	out := make(map[string]core.McpServerConfig)
	for name, entry := range doc.McpServers {
		out[name] = decodeCodexMcpEntry(entry)
	}
	return out, nil
}

// decodeCodexMcpEntry converts a generic TOML map into McpServerConfig. Note
// args/env are nested tables in TOML.
func decodeCodexMcpEntry(entry map[string]any) core.McpServerConfig {
	cfg := core.McpServerConfig{}
	if t, ok := entry["type"].(string); ok {
		cfg.Type = t
	}
	if c, ok := entry["command"].(string); ok {
		cfg.Command = c
	}
	if args, ok := entry["args"].([]any); ok {
		cfg.Args = make([]string, 0, len(args))
		for _, a := range args {
			if s, ok := a.(string); ok {
				cfg.Args = append(cfg.Args, s)
			}
		}
	}
	if env, ok := entry["env"].(map[string]any); ok {
		cfg.Env = make(map[string]string, len(env))
		for k, v := range env {
			if s, ok := v.(string); ok {
				cfg.Env[k] = s
			}
		}
	}
	if u, ok := entry["url"].(string); ok {
		cfg.URL = u
	}
	return cfg
}

// upsertCodexMcpServer rewrites config.toml text so [mcp_servers.<name>]
// reflects cfg, preserving every other section.
func upsertCodexMcpServer(configText, name string, cfg core.McpServerConfig) (string, error) {
	root := make(map[string]any)
	if configText != "" {
		if err := toml.Unmarshal([]byte(configText), &root); err != nil {
			return "", fmt.Errorf("codex: parse config.toml: %w", err)
		}
	}

	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}

	// Build the entry. args → array of strings; env → nested table.
	entry := make(map[string]any)
	if cfg.Type != "" {
		entry["type"] = cfg.Type
	}
	if cfg.Command != "" {
		entry["command"] = cfg.Command
	}
	if len(cfg.Args) > 0 {
		args := make([]any, len(cfg.Args))
		for i, a := range cfg.Args {
			args[i] = a
		}
		entry["args"] = args
	}
	if len(cfg.Env) > 0 {
		env := make(map[string]any, len(cfg.Env))
		for k, v := range cfg.Env {
			env[k] = v
		}
		entry["env"] = env
	}
	if cfg.URL != "" {
		entry["url"] = cfg.URL
	}

	servers[name] = entry
	root["mcp_servers"] = servers

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		return "", fmt.Errorf("codex: encode config.toml: %w", err)
	}
	return buf.String(), nil
}

// ListMcpServers reads [mcp_servers.*] from $CODEX_HOME/config.toml.
// Implements core.McpConfigManager.
func (a *Agent) ListMcpServers(ctx context.Context) (map[string]core.McpServerConfig, error) {
	a.mu.RLock()
	codexHome := a.codexHome
	a.mu.RUnlock()
	return listCodexMcpServers(codexHome)
}

// SaveMcpServer upserts one [mcp_servers.<name>] entry, preserving all other
// sections. Implements core.McpConfigManager.
func (a *Agent) SaveMcpServer(_ context.Context, name string, cfg core.McpServerConfig) error {
	if name == "" {
		return fmt.Errorf("codex: mcp server name is required")
	}
	if err := validateCodexMcpServer(cfg); err != nil {
		return err
	}
	a.mu.RLock()
	codexHome := a.codexHome
	a.mu.RUnlock()

	home, err := resolveCodexHomeForConfig(codexHome)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	configPath := filepath.Join(home, "config.toml")
	old, _ := os.ReadFile(configPath)

	updated, err := upsertCodexMcpServer(string(old), name, cfg)
	if err != nil {
		return err
	}
	// Validate the produced TOML parses before writing — never persist broken config.
	var probe map[string]any
	if err := toml.Unmarshal([]byte(updated), &probe); err != nil {
		return fmt.Errorf("codex: rejected config.toml (would not parse): %w", err)
	}
	if err := core.AtomicWriteFile(configPath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("codex: write config.toml: %w", err)
	}
	return nil
}

// DeleteMcpServer removes [mcp_servers.<name>]. Idempotent. Implements
// core.McpConfigManager.
func (a *Agent) DeleteMcpServer(_ context.Context, name string) error {
	a.mu.RLock()
	codexHome := a.codexHome
	a.mu.RUnlock()

	home, err := resolveCodexHomeForConfig(codexHome)
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, "config.toml")
	old, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	root := make(map[string]any)
	if err := toml.Unmarshal(old, &root); err != nil {
		return fmt.Errorf("codex: parse config.toml: %w", err)
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	if _, exists := servers[name]; !exists {
		return nil
	}
	delete(servers, name)
	if len(servers) == 0 {
		delete(root, "mcp_servers")
	} else {
		root["mcp_servers"] = servers
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		return fmt.Errorf("codex: encode config.toml: %w", err)
	}
	if err := core.AtomicWriteFile(configPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("codex: write config.toml: %w", err)
	}
	return nil
}

// validateCodexMcpServer mirrors the claudecode validation. Defined locally to
// keep the codex package self-contained (claudecode's version is private).
func validateCodexMcpServer(cfg core.McpServerConfig) error {
	switch normalizeType(cfg.Type) {
	case "sse", "http":
		if cfg.URL == "" {
			return fmt.Errorf("mcp server type %q requires a url", cfg.Type)
		}
	default:
		if cfg.Command == "" {
			return fmt.Errorf("stdio mcp server requires a command")
		}
	}
	return nil
}

// normalizeType lowercases + trims the type field; empty → "stdio".
func normalizeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return "stdio"
	}
	return t
}
