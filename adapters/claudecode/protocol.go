// Claude Code protocol helpers — migrated from the upstream in-repo
// agent/claudecode package (originally cc-connect) and adapted to the new
// runtime contract.
//
// This file holds the pure, unit-testable protocol pieces of the Claude Code
// CLI: stream-json event subtype parsing, usage extraction, permission-mode
// normalization, tool input summarization and env/arg helpers. Nothing here
// imports the old core/IM packages or net/http.
package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// normalizeEffort maps user-friendly aliases to Claude CLI --effort values.
func normalizeEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ""
	case "low":
		return "low"
	case "medium", "med":
		return "medium"
	case "high":
		return "high"
	case "max":
		return "max"
	default:
		return ""
	}
}

// normalizePermissionMode maps user-friendly aliases to Claude CLI values.
func normalizePermissionMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "acceptedits", "accept-edits", "accept_edits", "edit":
		return "acceptEdits"
	case "plan":
		return "plan"
	case "auto":
		return "auto"
	case "bypasspermissions", "bypass-permissions", "bypass_permissions",
		"yolo":
		return "bypassPermissions"
	case "dontask", "dont-ask", "dont_ask":
		return "dontAsk"
	default:
		return "default"
	}
}

// isCompactionResult reports whether a `type:"result"` event is actually a
// mid-turn compaction notification. Claude Code uses the value `compact` in
// newer CLI versions and `compaction` in older ones; we accept both to be
// safe across CLI rollouts (issue #481).
func isCompactionResult(raw map[string]any) bool {
	return resultSubtype(raw) == "compact" || resultSubtype(raw) == "compaction"
}

// resultSubtype extracts the optional `subtype` field from a result event
// payload. Empty string when missing.
func resultSubtype(raw map[string]any) string {
	if s, ok := raw["subtype"].(string); ok {
		return s
	}
	return ""
}

// parseClaudeUsage extracts the four token counts Claude reports per API call.
// Missing fields default to zero.
func parseClaudeUsage(usage map[string]any) (input, output, cacheCreation, cacheRead int) {
	if v, ok := usage["input_tokens"].(float64); ok {
		input = int(v)
	}
	if v, ok := usage["output_tokens"].(float64); ok {
		output = int(v)
	}
	if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
		cacheCreation = int(v)
	}
	if v, ok := usage["cache_read_input_tokens"].(float64); ok {
		cacheRead = int(v)
	}
	return
}

// summarizeInput produces a short human-readable description of tool input.
// Used for the canonical permission event Detail.
func summarizeInput(tool string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}

	switch tool {
	case "Read", "Edit", "Write":
		if fp, ok := m["file_path"].(string); ok {
			return fp
		}
	case "Bash":
		if cmd, ok := m["command"].(string); ok {
			return cmd
		}
	case "Grep":
		if p, ok := m["pattern"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := m["pattern"].(string); ok {
			return p
		}
		if p, ok := m["glob_pattern"].(string); ok {
			return p
		}
	}

	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func isClaudeEditTool(toolName string) bool {
	switch toolName {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return true
	default:
		return false
	}
}

// filterEnv returns a copy of env with entries matching the given key removed.
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// truncateStr truncates s to maxLen characters, appending "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// writeTempAppendPromptFile writes the append-system-prompt content to a
// per-spawn temp file, returning the path. A unique name from CreateTemp
// avoids two concurrent sessions overwriting each other. The caller is
// responsible for removing the file on session Close.
func writeTempAppendPromptFile(ccDataDir, content string) (string, error) {
	base := ccDataDir
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "agent-prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "agent-gateway-system-*.md")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	// os.CreateTemp defaults to mode 0600; the spawned agent may run as a
	// different user inside the sandbox, so mirror the shared-prompt file mode.
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// claudeLaunchParams carries the static launch-time options needed to build
// the Claude CLI argv. It is a struct so buildClaudeArgs stays unit-testable
// without launching a process.
type claudeLaunchParams struct {
	model            string
	effort           string
	mode             string
	sessionID        string
	systemPrompt     string
	appendPromptFile string
	allowedTools     []string
	disallowedTools  []string
	pluginDirs       []string
	maxContextTokens int
}

// buildClaudeArgs maps the session state to a Claude CLI argv for
// --input-format stream-json (the persistent-process protocol).
func buildClaudeArgs(p claudeLaunchParams) []string {
	innerArgs := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--replay-user-messages",
		"--verbose",
	}

	if p.mode != "" && p.mode != "default" {
		innerArgs = append(innerArgs, "--permission-mode", p.mode)
	}
	if p.sessionID != "" {
		innerArgs = append(innerArgs, "--resume", p.sessionID)
	}
	if len(p.allowedTools) > 0 {
		innerArgs = append(innerArgs, "--allowedTools", strings.Join(p.allowedTools, ","))
	}
	if len(p.disallowedTools) > 0 {
		innerArgs = append(innerArgs, "--disallowedTools", strings.Join(p.disallowedTools, ","))
	}
	for _, dir := range p.pluginDirs {
		innerArgs = append(innerArgs, "--plugin-dir", dir)
	}
	if p.systemPrompt != "" {
		innerArgs = append(innerArgs, "--system-prompt", p.systemPrompt)
	}
	if p.appendPromptFile != "" {
		innerArgs = append(innerArgs, "--append-system-prompt-file", p.appendPromptFile)
	}
	if p.effort != "" {
		innerArgs = append(innerArgs, "--effort", p.effort)
	}
	if p.maxContextTokens > 0 {
		innerArgs = append(innerArgs, "--max-context-tokens", strconv.Itoa(p.maxContextTokens))
	}
	if p.model != "" {
		innerArgs = append(innerArgs, "--model", p.model)
	}
	return innerArgs
}

// splitCommand splits "bin arg1 arg2" into the binary and its extra args.
func splitCommand(cmd string) (bin string, args []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "claude", nil
	}
	return parts[0], parts[1:]
}

// envOrDefault returns the value of env key or fallback when empty.
func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// splitEnv splits a "K=V,K2=V2" (or "K=V K2=V2") string into KEY=VALUE pairs.
func splitEnv(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part != "" && strings.Contains(part, "=") {
			out = append(out, part)
		}
	}
	return out
}

// mergeEnv returns base env with entries from extra overriding same-key
// entries. This prevents duplicate keys (e.g. two PATH entries) which would
// make the override silently ignored on Linux (getenv returns the first match).
func mergeEnv(base, extra []string) []string {
	keys := make(map[string]bool, len(extra))
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			keys[k] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok && keys[k] {
			continue
		}
		merged = append(merged, e)
	}
	return append(merged, extra...)
}

// redactArgs returns a copy of args with values after sensitive flag names
// masked, so secrets never land in logs.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)

	sensitiveFlags := []string{
		"--api-key", "--api_key", "--apikey",
		"--token", "--secret", "--password",
		"-k",
	}

	for i := 0; i < len(out); i++ {
		arg := strings.ToLower(out[i])
		for _, f := range sensitiveFlags {
			if strings.HasPrefix(arg, f+"=") {
				out[i] = out[i][:strings.Index(out[i], "=")+1] + "***"
				break
			}
			if arg == f && i+1 < len(out) {
				out[i+1] = "***"
				i++
				break
			}
		}
	}
	return out
}
