package codex

import (
	"strings"
	"time"
)

// Options configures a native Codex exec session.
type Options struct {
	Command            []string
	Env                []string
	WorkDir            string
	Model              string
	ReasoningEffort    string
	Mode               string
	ResumeID           string
	BaseURL            string
	ModelProvider      string
	SystemPrompt       string
	AppendSystemPrompt string
	CloseTimeout       time.Duration
}

// NormalizeOptions applies Codex-native defaults without Gateway dependencies.
func NormalizeOptions(opts Options) Options {
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		opts.Command = []string{"codex"}
	} else {
		opts.Command = append([]string(nil), opts.Command...)
	}
	opts.Mode = normalizeMode(opts.Mode)
	opts.ReasoningEffort = normalizeEffort(opts.ReasoningEffort)
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 8 * time.Second
	}
	return opts
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto-edit", "autoedit", "auto_edit", "edit":
		return "auto-edit"
	case "full-auto", "fullauto", "full_auto", "auto":
		return "full-auto"
	case "yolo", "bypass", "dangerously-bypass":
		return "yolo"
	default:
		return "suggest"
	}
}

func normalizeEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return "low"
	case "medium", "med":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "very-high":
		return "xhigh"
	default:
		return ""
	}
}
