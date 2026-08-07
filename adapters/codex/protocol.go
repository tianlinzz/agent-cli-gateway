// Codex protocol helpers — migrated from the upstream in-repo agent/codex
// package (originally cc-connect) and adapted to the new runtime contract.
//
// This file holds the pure, unit-testable protocol pieces of the Codex `exec`
// backend: JSONL stream line splitting, native stream event field extraction,
// prompt building, env/arg redaction helpers, and reading the native codex
// rollout token-count records into the canonical runtime.Usage. Nothing here
// imports the old core/IM packages or net/http.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

const codexRolloutTailBytes int64 = 1 << 20

// codexToolNames maps native codex stream item types to display tool names.
var codexToolNames = map[string]string{
	"web_search":       "WebSearch",
	"file_search":      "FileSearch",
	"code_interpreter": "CodeInterpreter",
	"computer_use":     "ComputerUse",
	"mcp_tool":         "MCP",
}

// readJSONLines splits an r by newline and calls handle for every non-empty
// line, mirroring the codex exec --json stdout format.
func readJSONLines(r io.Reader, handle func([]byte) error) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if err := handle(line); err != nil {
				return err
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

// extractItemText extracts text from an item's array field (e.g. "summary" or
// "content"). It looks for elements matching elementType and concatenates
// their "text" fields. Falls back to the item's top-level "text" field.
func extractItemText(item map[string]any, arrayField, elementType string) string {
	if arr, ok := item[arrayField].([]any); ok {
		var parts []string
		for _, elem := range arr {
			m, ok := elem.(map[string]any)
			if !ok {
				continue
			}
			if elementType != "" {
				if t, _ := m["type"].(string); t != elementType {
					continue
				}
			}
			if t, _ := m["text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	text, _ := item["text"].(string)
	return text
}

// codexExtractToolInput extracts a human-readable input from a Codex tool item.
// For web_search, it reads action.queries[] or falls back to the top-level
// query.
func codexExtractToolInput(item map[string]any) string {
	if action, ok := item["action"].(map[string]any); ok {
		if queries, ok := action["queries"].([]any); ok && len(queries) > 0 {
			var parts []string
			for _, q := range queries {
				if s, ok := q.(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
		if q, _ := action["query"].(string); q != "" {
			return q
		}
	}
	if q, _ := item["query"].(string); q != "" {
		return q
	}
	if n, _ := item["name"].(string); n != "" {
		return n
	}
	return ""
}

// codexToolSuccess reports whether a tool item completed successfully.
func codexToolSuccess(status string, exitCode *int) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	if exitCode != nil {
		return *exitCode == 0
	}
	return s == "completed" || s == "success" || s == "succeeded" || s == "ok"
}

// toolCallArgs turns a raw tool input string into canonical ToolCall.Arguments.
// function_call arguments are a JSON object when present; anything that does
// not parse as an object is carried verbatim under "arguments".
func toolCallArgs(kind, input string) map[string]any {
	if kind == "function_call" {
		var m map[string]any
		if err := json.Unmarshal([]byte(input), &m); err == nil && m != nil {
			return m
		}
		return map[string]any{"arguments": input}
	}
	// command_execution: the whole input is the shell command.
	return map[string]any{"command": input}
}

// buildCodexPromptPreamble joins the system and appended prompts into a single
// preamble used only for the first (fresh) turn.
func buildCodexPromptPreamble(systemPrompt, appendPrompt string) string {
	var sections []string
	if systemPrompt = strings.TrimSpace(systemPrompt); systemPrompt != "" {
		sections = append(sections, "Project system prompt:\n"+systemPrompt)
	}
	if appendPrompt = strings.TrimSpace(appendPrompt); appendPrompt != "" {
		sections = append(sections, "Additional project instructions:\n"+appendPrompt)
	}
	return strings.Join(sections, "\n\n")
}

// prependPromptPreamble wraps a fresh prompt with the project preamble so the
// CLI sees the instructions before the first user message.
func prependPromptPreamble(prompt string, preamble string) string {
	preamble = strings.TrimSpace(preamble)
	if preamble == "" {
		return prompt
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "Before answering, follow these project-level instructions for this gateway session. They are not user content.\n\n" + preamble
	}
	return "Before answering, follow these project-level instructions for this gateway session. They are not user content.\n\n" + preamble + "\n\n---\n\nUser message:\n" + prompt
}

// promptFromInput serializes a canonical Input turn into the prompt passed to
// `codex exec`. On resume codex owns the full history natively, so only the
// last user message is sent; on a fresh turn the preamble (if any) plus the
// role-labeled messages are sent.
func promptFromInput(in runtime.Input, isResume bool, preamble string) string {
	var userParts []string
	var otherParts []string
	for _, m := range in.Messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		switch m.Role {
		case "user":
			userParts = append(userParts, content)
		case "system":
			otherParts = append(otherParts, "System instructions:\n"+content)
		case "assistant":
			otherParts = append(otherParts, "Assistant:\n"+content)
		case "tool":
			otherParts = append(otherParts, "Tool result:\n"+content)
		default:
			otherParts = append(otherParts, content)
		}
	}
	if isResume {
		if len(userParts) == 0 {
			return ""
		}
		return userParts[len(userParts)-1]
	}

	prompt := ""
	if len(otherParts) == 0 {
		// Pure user conversation: the preamble's "User message:" heading carries
		// the role, so the raw content is used directly.
		prompt = strings.Join(userParts, "\n\n")
	} else {
		all := append([]string(nil), otherParts...)
		for _, u := range userParts {
			all = append(all, "User:\n"+u)
		}
		prompt = strings.Join(all, "\n\n")
	}
	if preamble != "" {
		return prependPromptPreamble(prompt, preamble)
	}
	return prompt
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

// getenvFromList returns the last matching KEY=VALUE entry from env.
func getenvFromList(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		entry := env[i]
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(entry, prefix))
		}
	}
	return ""
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

// truncate limits a string to maxRunes runes, appending an ellipsis.
func truncate(s string, maxRunes int) string {
	if len([]rune(s)) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}

// ---------------------------------------------------------------------------
// Native rollout token-count parsing → canonical runtime.Usage.
// Codex writes a per-thread rollout JSONL under CODEX_HOME/sessions; the
// `event_msg`/`token_count` records carry the per-turn token accounting.
// ---------------------------------------------------------------------------

type codexSnakeTokenUsage struct {
	TotalTokens           int `json:"total_tokens"`
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

func usageFromSnake(usage codexSnakeTokenUsage) *runtime.Usage {
	if usage.TotalTokens <= 0 && usage.InputTokens <= 0 && usage.OutputTokens <= 0 {
		return nil
	}
	return &runtime.Usage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
	}
}

// loadContextUsageFromRollout resolves the native session file for sessionID
// (cached at cachedPath when non-empty) and parses the last token_count.
func loadContextUsageFromRollout(extraEnv []string, sessionID, cachedPath string) (*runtime.Usage, string, error) {
	path := strings.TrimSpace(cachedPath)
	if path != "" {
		usage, err := readContextUsageFromRollout(path)
		if err == nil && usage != nil {
			return usage, path, nil
		}
	}

	codexHome, err := resolveCodexHome(extraEnv)
	if err != nil {
		return nil, "", err
	}
	path = findSessionFileInCodexHome(codexHome, sessionID)
	if path == "" {
		return nil, "", fmt.Errorf("session file not found for %s", sessionID)
	}
	usage, err := readContextUsageFromRollout(path)
	if err != nil {
		return nil, path, err
	}
	if usage == nil {
		return nil, path, fmt.Errorf("context usage not found in rollout")
	}
	return usage, path, nil
}

func resolveCodexHome(extraEnv []string) (string, error) {
	if value := getenvFromList(extraEnv, "CODEX_HOME"); value != "" {
		return strings.TrimSpace(value), nil
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		return value, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(homeDir, ".codex"), nil
}

func findSessionFileInCodexHome(codexHome, sessionID string) string {
	if strings.TrimSpace(codexHome) == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}

	pattern := filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*"+sessionID+".jsonl")
	if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
		sort.Strings(matches)
		return matches[len(matches)-1]
	}

	sessionsDir := filepath.Join(codexHome, "sessions")
	var found string
	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || found != "" {
			return nil
		}
		if strings.Contains(filepath.Base(path), sessionID) {
			found = path
		}
		return nil
	})
	return found
}

func readContextUsageFromRollout(path string) (*runtime.Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if usage, err := readContextUsageFromRolloutTail(f); err != nil {
		return nil, err
	} else if usage != nil {
		return usage, nil
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return scanContextUsageFromRollout(f)
}

func readContextUsageFromRolloutTail(f *os.File) (*runtime.Usage, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 {
		return nil, nil
	}

	start := int64(0)
	if info.Size() > codexRolloutTailBytes {
		start = info.Size() - codexRolloutTailBytes
	}
	buf := make([]byte, int(info.Size()-start))
	n, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]
	if start > 0 {
		if idx := bytes.IndexByte(buf, '\n'); idx >= 0 {
			buf = buf[idx+1:]
		}
	}
	return parseContextUsageFromRolloutBytes(buf), nil
}

func parseContextUsageFromRolloutBytes(data []byte) *runtime.Usage {
	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if usage := parseContextUsageFromRolloutLine(line); usage != nil {
			return usage
		}
	}
	return nil
}

func scanContextUsageFromRollout(r io.Reader) (*runtime.Usage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)

	var last *runtime.Usage
	for scanner.Scan() {
		if usage := parseContextUsageFromRolloutLine(scanner.Bytes()); usage != nil {
			last = usage
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return last, nil
}

func parseContextUsageFromRolloutLine(line []byte) *runtime.Usage {
	var entry struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return nil
	}
	if entry.Type != "event_msg" {
		return nil
	}

	var payload struct {
		Type string `json:"type"`
		Info *struct {
			TotalTokenUsage    codexSnakeTokenUsage `json:"total_token_usage"`
			LastTokenUsage     codexSnakeTokenUsage `json:"last_token_usage"`
			ModelContextWindow int                  `json:"model_context_window"`
		} `json:"info"`
	}
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		return nil
	}
	if payload.Type != "token_count" || payload.Info == nil {
		return nil
	}
	return usageFromSnake(payload.Info.LastTokenUsage)
}
