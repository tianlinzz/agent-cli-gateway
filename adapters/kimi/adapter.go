// Package kimi adapts native Kimi Code sessions to the Gateway runtime.
package kimi

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	native "github.com/tianlinzz/agent-cli-gateway/agent/kimi"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() { _ = runtime.Register("kimi", New) }

// Options is trusted deployment configuration for the Kimi adapter.
type Options struct {
	Command    string
	Env        []string
	WorkDir    string
	Model      string
	Mode       string
	Timeout    time.Duration
	Permission string
	// InjectSystemPrompt forwards caller-supplied system role messages into the
	// native prompt at each turn.
	InjectSystemPrompt bool
}

type nativeSession interface {
	Send(context.Context, native.Input) error
	Events() <-chan native.Event
	Abort(context.Context) error
	Close(context.Context) error
	NativeSessionID() string
}

type starter func(context.Context, native.Options) (nativeSession, error)

// Adapter implements runtime.AgentAdapter as a thin native bridge.
type Adapter struct {
	opts  Options
	start starter
}

// New constructs the registered adapter from worker-owned environment.
func New(ctx context.Context, _ string) (runtime.AgentAdapter, error) {
	opts := Options{
		Command:            envOrDefault("CC_GATEWAY_KIMI_COMMAND", "kimi"),
		WorkDir:            envOrDefault("GW_WORKSPACE_DIR", "/workspace"),
		Model:              envOrDefault("CC_GATEWAY_KIMI_MODEL", ""),
		Mode:               envOrDefault("CC_GATEWAY_KIMI_MODE", "default"),
		Permission:         envOrDefault("CC_GATEWAY_KIMI_PERMISSION", "auto"),
		InjectSystemPrompt: envBool("CC_GATEWAY_KIMI_INJECT_SYSTEM_PROMPT"),
	}
	if raw := envOrDefault("CC_GATEWAY_KIMI_ENV", ""); raw != "" {
		opts.Env = splitEnv(raw)
	}
	if raw := envOrDefault("CC_GATEWAY_KIMI_TIMEOUT_SECS", ""); raw != "" {
		if duration, err := time.ParseDuration(raw); err == nil {
			opts.Timeout = duration
		} else if seconds, err := strconv.Atoi(raw); err == nil {
			opts.Timeout = time.Duration(seconds) * time.Second
		}
	}
	return newAdapter(opts, startNative), nil
}

// NewAdapter constructs an adapter from explicit trusted options.
func NewAdapter(opts Options) (*Adapter, error) {
	return newAdapter(opts, startNative), nil
}

func startNative(ctx context.Context, options native.Options) (nativeSession, error) {
	return native.Start(ctx, options)
}

func newAdapter(opts Options, start starter) *Adapter {
	if strings.TrimSpace(opts.Command) == "" {
		opts.Command = "kimi"
	}
	return &Adapter{opts: opts, start: start}
}

// Describe returns the public Kimi model descriptor.
func (a *Adapter) Describe(context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{
		ModelID:       "kimi",
		DisplayName:   "Kimi",
		Description:   "Kimi Code CLI via persistent ACP",
		LifecycleMode: runtime.LifecyclePersistentProcess,
		Capabilities: runtime.Capabilities{
			Streaming: true, ToolCalls: true, Reasoning: true,
			Resume: true, MultiTurn: true,
		},
	}, nil
}

// Start converts runtime metadata and trusted config into native options.
func (a *Adapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	resumeID := strings.TrimSpace(req.Metadata["kimi_session_id"])
	if resumeID == "" {
		resumeID = strings.TrimSpace(req.Metadata["native_session_id"])
	}
	start := a.start
	if start == nil {
		start = startNative
	}
	session, err := start(ctx, native.Options{
		Command: splitCommand(a.opts.Command),
		Env:     append([]string(nil), a.opts.Env...), WorkDir: a.opts.WorkDir,
		Model: a.opts.Model, Mode: a.opts.Mode, Permission: a.opts.Permission, ResumeID: resumeID,
		Timeout: a.opts.Timeout,
	})
	if err != nil {
		return nil, err
	}
	return wrapSession(session, a.opts.InjectSystemPrompt), nil
}

func splitCommand(command string) []string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return []string{"kimi"}
	}
	return parts
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func splitEnv(raw string) []string {
	var result []string
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n'
	}) {
		if value = strings.TrimSpace(value); strings.Contains(value, "=") {
			result = append(result, value)
		}
	}
	return result
}
