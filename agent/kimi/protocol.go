package kimi

import (
	"encoding/json"
	"fmt"
	"strings"
)

func extractResumeID(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "To resume this session:") {
			continue
		}
		fields := strings.Fields(line)
		for i, field := range fields {
			if (field == "-r" || field == "--resume") && i+1 < len(fields) {
				return strings.Trim(fields[i+1], "'\"")
			}
		}
	}
	return ""
}

func toolArgs(raw string) map[string]any {
	var result map[string]any
	if json.Unmarshal([]byte(raw), &result) == nil && result != nil {
		return result
	}
	return map[string]any{"arguments": raw}
}

func usageFromValue(value any) *Usage {
	raw, _ := value.(map[string]any)
	u := &Usage{InputTokens: kimiInt(raw["input_tokens"]), OutputTokens: kimiInt(raw["output_tokens"]), TotalTokens: kimiInt(raw["total_tokens"])}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if u.TotalTokens == 0 {
		return nil
	}
	return u
}

func kimiInt(value any) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return 0
}
func protocolError(format string, args ...any) error { return fmt.Errorf("kimi: "+format, args...) }
