package codex

import (
	"encoding/json"
	"fmt"
	"strings"
)

func toolFromItem(item map[string]any) (ToolCall, bool) {
	id := stringValue(item["id"])
	if id == "" {
		return ToolCall{}, false
	}
	switch kind := stringValue(item["type"]); kind {
	case "commandExecution":
		return ToolCall{ID: id, Name: "Bash", Arguments: map[string]any{"command": stringValue(item["command"])}}, true
	case "fileChange":
		return ToolCall{ID: id, Name: "FileChange", Arguments: map[string]any{"changes": item["changes"]}}, true
	case "mcpToolCall":
		return ToolCall{ID: id, Name: joinToolName(stringValue(item["server"]), stringValue(item["tool"])), Arguments: objectValue(item["arguments"])}, true
	case "dynamicToolCall":
		return ToolCall{ID: id, Name: stringValue(item["tool"]), Arguments: objectValue(item["arguments"])}, true
	default:
		return ToolCall{}, false
	}
}

func toolResult(item map[string]any) (string, bool) {
	status := strings.ToLower(stringValue(item["status"]))
	isError := status == "failed" || status == "declined" || status == "cancelled"
	switch stringValue(item["type"]) {
	case "commandExecution":
		if exitCode, ok := numberValue(item["exitCode"]); ok && exitCode != 0 {
			isError = true
		}
		return truncate(strings.TrimSpace(stringValue(item["aggregatedOutput"])), 500), isError
	case "fileChange":
		return encodeResult(item["changes"]), isError
	case "mcpToolCall", "dynamicToolCall":
		if errText := encodeResult(item["error"]); errText != "" && errText != "null" {
			return truncate(errText, 500), true
		}
		result := item["result"]
		if result == nil {
			result = item["content"]
		}
		return truncate(encodeResult(result), 500), isError
	default:
		return "", isError
	}
}

func joinToolName(server, tool string) string {
	if server == "" {
		return tool
	}
	if tool == "" {
		return server
	}
	return server + "/" + tool
}

func encodeResult(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func usageFromCamelMap(raw map[string]any) *Usage {
	usage := &Usage{
		InputTokens:  intNumber(raw["inputTokens"]),
		OutputTokens: intNumber(raw["outputTokens"]),
		TotalTokens:  intNumber(raw["totalTokens"]),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.TotalTokens == 0 {
		return nil
	}
	return usage
}

func numberValue(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func intNumber(value any) int {
	number, _ := numberValue(value)
	return number
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

func protocolError(format string, args ...any) error { return fmt.Errorf("codex: "+format, args...) }
