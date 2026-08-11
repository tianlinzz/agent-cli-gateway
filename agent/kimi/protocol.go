package kimi

import (
	"encoding/json"
	"fmt"
	"strings"
)

func toolFromUpdate(update map[string]any) ToolCall {
	name := stringValue(update["title"])
	if name == "" {
		name = stringValue(update["kind"])
	}
	arguments := objectValue(update["rawInput"])
	if arguments == nil {
		arguments = map[string]any{}
	}
	return ToolCall{ID: stringValue(update["toolCallId"]), Name: name, Arguments: arguments}
}

func toolContent(value any) string {
	parts, _ := value.([]any)
	var result []string
	for _, partValue := range parts {
		part := objectValue(partValue)
		content := objectValue(part["content"])
		if stringValue(content["type"]) == "text" && stringValue(content["text"]) != "" {
			result = append(result, stringValue(content["text"]))
		}
	}
	return strings.Join(result, "\n")
}

func allowedOption(value any) string {
	options, _ := value.([]any)
	for _, optionValue := range options {
		option := objectValue(optionValue)
		kind := strings.ToLower(stringValue(option["kind"]))
		id := stringValue(option["optionId"])
		if id == "" {
			id = stringValue(option["id"])
		}
		if id != "" && (strings.Contains(kind, "allow") || strings.Contains(strings.ToLower(id), "approve")) {
			return id
		}
	}
	return ""
}

func normalizeStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "cancelled", "canceled":
		return "cancelled"
	case "max_tokens", "max_output_tokens":
		return "length"
	default:
		return "end_turn"
	}
}

func objectValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func intValue(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func protocolError(format string, args ...any) error { return fmt.Errorf("kimi: "+format, args...) }
