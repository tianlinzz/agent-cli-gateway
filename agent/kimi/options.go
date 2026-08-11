package kimi

import (
	"strings"
	"time"
)

type Options struct {
	Command    []string
	Env        []string
	WorkDir    string
	Model      string
	Mode       string
	Permission string
	ResumeID   string
	Timeout    time.Duration
}

func NormalizeOptions(opts Options) Options {
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		opts.Command = []string{"kimi"}
	} else {
		opts.Command = append([]string(nil), opts.Command...)
	}
	switch strings.ToLower(strings.TrimSpace(opts.Mode)) {
	case "plan":
		opts.Mode = "plan"
	case "quiet":
		opts.Mode = "quiet"
	default:
		opts.Mode = "default"
	}
	switch strings.ToLower(strings.TrimSpace(opts.Permission)) {
	case "ask":
		opts.Permission = "ask"
	case "deny":
		opts.Permission = "deny"
	default:
		opts.Permission = "auto"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	return opts
}
