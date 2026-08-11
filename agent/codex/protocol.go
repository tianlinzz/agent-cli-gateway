package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var toolNames = map[string]string{
	"web_search": "WebSearch", "file_search": "FileSearch",
	"code_interpreter": "CodeInterpreter", "computer_use": "ComputerUse", "mcp_tool": "MCP",
}

func extractItemText(item map[string]any, field, elementType string) string {
	if values, ok := item[field].([]any); ok {
		var parts []string
		for _, value := range values {
			block, ok := value.(map[string]any)
			if !ok || (elementType != "" && block["type"] != elementType) {
				continue
			}
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	text, _ := item["text"].(string)
	return text
}

func toolArguments(kind, input string) map[string]any {
	if kind == "function_call" {
		var result map[string]any
		if json.Unmarshal([]byte(input), &result) == nil && result != nil {
			return result
		}
		return map[string]any{"arguments": input}
	}
	return map[string]any{"command": input}
}

func toolSuccess(status string, exitCode *int) bool {
	if exitCode != nil {
		return *exitCode == 0
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "success", "succeeded", "ok":
		return true
	default:
		return false
	}
}

func toolInput(item map[string]any) string {
	if action, ok := item["action"].(map[string]any); ok {
		if values, ok := action["queries"].([]any); ok {
			var queries []string
			for _, value := range values {
				if query, _ := value.(string); query != "" {
					queries = append(queries, query)
				}
			}
			if len(queries) > 0 {
				return strings.Join(queries, "\n")
			}
		}
		if query, _ := action["query"].(string); query != "" {
			return query
		}
	}
	for _, key := range []string{"query", "name"} {
		if value, _ := item[key].(string); value != "" {
			return value
		}
	}
	return ""
}

func usageFromMap(value any) *Usage {
	raw, _ := value.(map[string]any)
	usage := &Usage{InputTokens: intValue(raw["input_tokens"]), OutputTokens: intValue(raw["output_tokens"]), TotalTokens: intValue(raw["total_tokens"])}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.TotalTokens == 0 {
		return nil
	}
	return usage
}

func intValue(value any) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return 0
}

func joinSections(sections []string) string { return strings.Join(sections, "\n\n") }

func prependPreamble(prompt, preamble string) string {
	if strings.TrimSpace(preamble) == "" {
		return prompt
	}
	return "Before answering, follow these project-level instructions for this gateway session. They are not user content.\n\n" + preamble + "\n\n---\n\nUser message:\n" + strings.TrimSpace(prompt)
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

type snakeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ReadRolloutUsage reads the last native token_count record from a rollout.
func ReadRolloutUsage(path string) (*Usage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var last *Usage
	for scanner.Scan() {
		if usage := parseRolloutLine(scanner.Bytes()); usage != nil {
			last = usage
		}
	}
	return last, scanner.Err()
}

func parseRolloutLine(line []byte) *Usage {
	var entry struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &entry) != nil || entry.Type != "event_msg" {
		return nil
	}
	var payload struct {
		Type string `json:"type"`
		Info *struct {
			Last snakeUsage `json:"last_token_usage"`
		} `json:"info"`
	}
	if json.Unmarshal(entry.Payload, &payload) != nil || payload.Type != "token_count" || payload.Info == nil {
		return nil
	}
	usage := payload.Info.Last
	if usage.TotalTokens == 0 && usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return nil
	}
	return &Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
}

func findRollout(env []string, threadID string) string {
	home := envValue(env, "CODEX_HOME")
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	pattern := filepath.Join(home, "sessions", "*", "*", "*", "rollout-*"+threadID+".jsonl")
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	return ""
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if name, value, ok := strings.Cut(env[i], "="); ok && strings.EqualFold(name, key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func protocolError(format string, args ...any) error { return fmt.Errorf("codex: "+format, args...) }
