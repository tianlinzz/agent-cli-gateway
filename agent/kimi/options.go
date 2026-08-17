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
	// CloseTimeout bounds how long Abort and Close wait for an in-flight
	// prompt to settle before escalating to a process kill. It is NOT a turn
	// deadline: the per-turn deadline is enforced at the worker boundary
	// (O-F09b); the native session only bounds its settle/teardown waits.
	CloseTimeout time.Duration
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
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 8 * time.Second
	}
	return opts
}
