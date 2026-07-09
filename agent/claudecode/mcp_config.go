package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// Claude Code reads MCP server definitions from the top-level "mcpServers"
// object of ~/.claude.json (a large file that also holds project history and
// onboarding state — unrelated fields must be preserved on every write).
//
// On Windows, stdio servers whose command is npx/npm/yarn/pnpm/node/bun/deno
// must be wrapped with "cmd /c" so the spawned shell resolves the .cmd shim
// correctly. cc-switch's claude_mcp.rs applies the same rule, with a carve-out
// for WSL UNC paths (the agent process runs in WSL where shims resolve natively).
//
// This file mirrors that behaviour so MCP servers written by the management API
// work out-of-the-box on Windows — the gateway host OS.

// windowsWrapCommands is the set of commands that need cmd /c wrapping on
// Windows. Matched case-insensitively against the bare command stem.
var windowsWrapCommands = map[string]struct{}{
	"npx":  {},
	"npm":  {},
	"yarn": {},
	"pnpm": {},
	"node": {},
	"bun":  {},
	"deno": {},
}

// claudeMcpPath returns the path to ~/.claude.json. The parent dir is created
// on demand so a fresh install can be written to.
func claudeMcpPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claudecode: cannot determine home dir: %w", err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", fmt.Errorf("claudecode: mkdir %s: %w", home, err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// commandStem extracts the bare command name (no path, no .cmd/.exe/.bat
// suffix) from a command string, for matching against windowsWrapCommands.
// Suffix matching is case-insensitive because Windows filesystems are —
// "yarn.CMD", "npx.Exe" etc. are all the same binary.
func commandStem(cmd string) string {
	if cmd == "" {
		return ""
	}
	// Take the last path segment, handling BOTH separators so Windows paths
	// (C:\bin\node.exe) are parsed correctly even when this code runs on a
	// non-Windows host (e.g. a Linux container processing a config written on
	// Windows). filepath.Base only recognises the host OS separator.
	base := cmd
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	// Strip Windows executable extensions, case-insensitively.
	for _, suf := range []string{".cmd", ".exe", ".bat", ".com"} {
		if len(base) > len(suf) && strings.EqualFold(base[len(base)-len(suf):], suf) {
			base = base[:len(base)-len(suf)]
			break
		}
	}
	return strings.ToLower(base)
}

// isWslPath reports whether path is a WSL UNC mount (\\wsl$\... or
// \\wsl.localhost\...). Such paths are accessed from inside WSL, so the
// Windows cmd shim wrapping does not apply.
func isWslPath(p string) bool {
	lower := strings.ToLower(p)
	return strings.HasPrefix(lower, `\\wsl$\`) ||
		strings.HasPrefix(lower, `\\wsl.localhost\`)
}

// wrapForWindows applies the "cmd /c <command> <args...>" rewrite when:
//   - we are on Windows,
//   - the server is stdio (or type is unset, which also means stdio),
//   - the command stem is in windowsWrapCommands,
//   - the command itself is not already cmd, and
//   - there is no WSL UNC prefix on the command path.
//
// Returns the (command, args) pair to write into the mcpServers entry. Pure
// function — no filesystem access — so it is easy to unit-test on any OS.
func wrapForWindows(srv map[string]any) (string, []any) {
	cmd, _ := srv["command"].(string)
	if cmd == "" {
		return "", nil
	}
	rawArgs, _ := srv["args"].([]any)

	// Only stdio servers have a command at all; nothing to wrap for sse/http.
	if t, _ := srv["type"].(string); t == "sse" || t == "http" {
		return cmd, rawArgs
	}

	if runtime.GOOS != "windows" {
		return cmd, rawArgs
	}
	if isWslPath(cmd) {
		return cmd, rawArgs
	}
	stem := commandStem(cmd)
	if stem == "cmd" {
		return cmd, rawArgs // already wrapped
	}
	if _, need := windowsWrapCommands[stem]; !need {
		return cmd, rawArgs
	}

	// cmd /c <original command> <original args...>
	wrapped := make([]any, 0, len(rawArgs)+2)
	wrapped = append(wrapped, "/c", cmd)
	wrapped = append(wrapped, rawArgs...)
	return "cmd", wrapped
}

// stripUIFields removes keys that are UI-only metadata and should not be
// persisted into the live ~/.claude.json mcpServers entry. Mirrors cc-switch's
// strip_ui_fields so writes don't accumulate display cruft on disk.
func stripUIFields(srv map[string]any) {
	for _, k := range []string{"enabled", "source", "id", "name", "description", "tags", "homepage", "docs"} {
		delete(srv, k)
	}
	// cc-switch also unwraps a {"server": {...}} envelope.
	if inner, ok := srv["server"].(map[string]any); ok {
		for k, v := range inner {
			if _, exists := srv[k]; !exists {
				srv[k] = v
			}
		}
		delete(srv, "server")
	}
}

// validateMcpServer returns an error if the config is structurally invalid.
// stdio requires Command; sse/http require URL.
func validateMcpServer(srv core.McpServerConfig) error {
	switch strings.ToLower(strings.TrimSpace(srv.Type)) {
	case "sse", "http":
		if srv.URL == "" {
			return fmt.Errorf("mcp server type %q requires a url", srv.Type)
		}
	default: // stdio (empty or explicit)
		if srv.Command == "" {
			return fmt.Errorf("stdio mcp server requires a command")
		}
	}
	return nil
}

// ListMcpServers reads ~/.claude.json's mcpServers map. Implements
// core.McpConfigManager.
func (a *Agent) ListMcpServers(_ context.Context) (map[string]core.McpServerConfig, error) {
	path, err := claudeMcpPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]core.McpServerConfig{}, nil
		}
		return nil, fmt.Errorf("claudecode: read .claude.json: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("claudecode: parse .claude.json: %w", err)
	}

	out := make(map[string]core.McpServerConfig)
	servers, _ := root["mcpServers"].(map[string]any)
	for name, v := range servers {
		entry, _ := v.(map[string]any)
		if entry == nil {
			continue
		}
		out[name] = decodeMcpEntry(entry)
	}
	return out, nil
}

// decodeMcpEntry converts a raw JSON-shaped mcpServers entry into the typed
// McpServerConfig. Unknown fields are dropped on read.
func decodeMcpEntry(entry map[string]any) core.McpServerConfig {
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

// SaveMcpServer upserts one MCP server into ~/.claude.json's mcpServers,
// preserving all other entries and unrelated root fields. Implements
// core.McpConfigManager.
func (a *Agent) SaveMcpServer(_ context.Context, name string, cfg core.McpServerConfig) error {
	if name == "" {
		return fmt.Errorf("claudecode: mcp server name is required")
	}
	if err := validateMcpServer(cfg); err != nil {
		return err
	}

	path, err := claudeMcpPath()
	if err != nil {
		return err
	}

	root := make(map[string]any)
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("claudecode: parse .claude.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("claudecode: read .claude.json: %w", err)
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}

	// Build the entry as a generic map so we can run wrapForWindows / strip
	// helpers uniformly (they operate on map[string]any, matching cc-switch).
	entry := buildMcpEntry(cfg)
	stripUIFields(entry)
	cmd, wrappedArgs := wrapForWindows(entry)
	if cmd != "" {
		entry["command"] = cmd
	}
	if wrappedArgs != nil {
		entry["args"] = wrappedArgs
	}

	servers[name] = entry
	root["mcpServers"] = servers

	data, err := marshalSorted(root)
	if err != nil {
		return fmt.Errorf("claudecode: marshal .claude.json: %w", err)
	}
	if err := core.AtomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("claudecode: write .claude.json: %w", err)
	}
	return nil
}

// DeleteMcpServer removes one MCP server by name. Idempotent: deleting a
// missing name is success. Implements core.McpConfigManager.
func (a *Agent) DeleteMcpServer(_ context.Context, name string) error {
	path, err := claudeMcpPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to delete from
		}
		return fmt.Errorf("claudecode: read .claude.json: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("claudecode: parse .claude.json: %w", err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		return nil
	}
	if _, exists := servers[name]; !exists {
		return nil
	}
	delete(servers, name)
	root["mcpServers"] = servers

	data, err := marshalSorted(root)
	if err != nil {
		return fmt.Errorf("claudecode: marshal .claude.json: %w", err)
	}
	if err := core.AtomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("claudecode: write .claude.json: %w", err)
	}
	return nil
}

// buildMcpEntry converts the typed config to a JSON-shaped map for writing.
func buildMcpEntry(cfg core.McpServerConfig) map[string]any {
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
	return entry
}
