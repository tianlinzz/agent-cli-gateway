package kimi

import (
	"strings"
	"time"
)

type FlagSupport struct{ Print bool }

type Options struct {
	Command  []string
	Env      []string
	WorkDir  string
	Model    string
	Mode     string
	ResumeID string
	Timeout  time.Duration
	Flags    FlagSupport
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
	return opts
}
