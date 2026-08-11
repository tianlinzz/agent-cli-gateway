package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseUsage(raw map[string]any) (input, output, cacheCreation, cacheRead int) {
	input = number(raw["input_tokens"])
	output = number(raw["output_tokens"])
	cacheCreation = number(raw["cache_creation_input_tokens"])
	cacheRead = number(raw["cache_read_input_tokens"])
	return
}

func number(value any) int {
	switch value := value.(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	case int:
		return value
	default:
		return 0
	}
}

func isCompactionResult(raw map[string]any) bool {
	subtype, _ := raw["subtype"].(string)
	return subtype == "compact" || subtype == "compaction"
}

func summarizeInput(tool string, input map[string]any) string {
	switch tool {
	case "Read", "Edit", "Write":
		if value, _ := input["file_path"].(string); value != "" {
			return value
		}
	case "Bash":
		if value, _ := input["command"].(string); value != "" {
			return value
		}
	case "Grep":
		if value, _ := input["pattern"].(string); value != "" {
			return value
		}
	case "Glob":
		if value, _ := input["pattern"].(string); value != "" {
			return value
		}
		if value, _ := input["glob_pattern"].(string); value != "" {
			return value
		}
	}
	encoded, _ := json.Marshal(input)
	return string(encoded)
}

func isEditTool(name string) bool {
	switch name {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return true
	default:
		return false
	}
}

func toolResultText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if block, ok := item.(map[string]any); ok {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func permissionResponse(requestID string, allow bool, message string) map[string]any {
	var decision map[string]any
	if allow {
		decision = map[string]any{"behavior": "allow", "updatedInput": map[string]any{}}
	} else {
		if message == "" {
			message = "The user denied this tool use. Stop and wait for the user's instructions."
		}
		decision = map[string]any{"behavior": "deny", "message": message}
	}
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype": "success", "request_id": requestID, "response": decision,
		},
	}
}

func redactText(value string, secrets []string) string {
	redacted := value
	for _, secret := range secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "***")
		}
	}
	return redacted
}

func secretValues(env []string) []string {
	var secrets []string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "KEY") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "AUTH") {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func protocolError(format string, args ...any) error {
	return fmt.Errorf("claudecode: "+format, args...)
}
