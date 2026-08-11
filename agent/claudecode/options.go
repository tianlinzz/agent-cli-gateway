package claudecode

import (
	"strings"
	"time"
)

// Options configures one native Claude Code session.
type Options struct {
	Command            []string
	Env                []string
	WorkDir            string
	Model              string
	ReasoningEffort    string
	ResumeID           string
	Mode               string
	Permission         string
	SystemPrompt       string
	AppendSystemPrompt string
	AllowedTools       []string
	DisallowedTools    []string
	MaxContextTokens   int
	CloseTimeout       time.Duration
}

// NormalizeOptions applies native Claude defaults without reading Gateway
// configuration or process environment.
func NormalizeOptions(opts Options) Options {
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		opts.Command = []string{"claude"}
	} else {
		opts.Command = append([]string(nil), opts.Command...)
	}
	opts.Mode = normalizePermissionMode(opts.Mode)
	opts.ReasoningEffort = normalizeEffort(opts.ReasoningEffort)
	opts.Permission = strings.ToLower(strings.TrimSpace(opts.Permission))
	if opts.Permission == "" {
		opts.Permission = "auto"
	}
	opts.AllowedTools = cleanTools(opts.AllowedTools)
	opts.DisallowedTools = cleanTools(opts.DisallowedTools)
	if !containsTool(opts.DisallowedTools, "AskUserQuestion") {
		opts.DisallowedTools = append(opts.DisallowedTools, "AskUserQuestion")
	}
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 120 * time.Second
	}
	return opts
}

func cleanTools(tools []string) []string {
	cleaned := make([]string, 0, len(tools))
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool != "" && !containsTool(cleaned, tool) {
			cleaned = append(cleaned, tool)
		}
	}
	return cleaned
}

func containsTool(tools []string, want string) bool {
	for _, tool := range tools {
		if strings.EqualFold(strings.TrimSpace(tool), want) {
			return true
		}
	}
	return false
}

func normalizeEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
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

func normalizePermissionMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "acceptedits", "accept-edits", "accept_edits", "edit":
		return "acceptEdits"
	case "plan":
		return "plan"
	case "auto":
		return "auto"
	case "bypasspermissions", "bypass-permissions", "bypass_permissions", "yolo":
		return "bypassPermissions"
	case "dontask", "dont-ask", "dont_ask":
		return "dontAsk"
	default:
		return "default"
	}
}
