package codex

import (
	"strings"
	"time"
)

// Options configures a persistent native Codex app-server session.
type Options struct {
	Command            []string
	Env                []string
	WorkDir            string
	Model              string
	ReasoningEffort    string
	Permission         string
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
	opts.Permission = normalizePermission(opts.Permission)
	opts.ReasoningEffort = normalizeEffort(opts.ReasoningEffort)
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 8 * time.Second
	}
	return opts
}

func normalizePermission(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ask":
		return "ask"
	case "deny":
		return "deny"
	default:
		return "auto"
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
