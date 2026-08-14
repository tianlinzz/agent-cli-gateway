// Package claudecode adapts native Claude Code events and lifecycle to the
// transport-neutral Gateway runtime contract.
package claudecode

import (
	"context"
	"sort"
	"strings"

	native "github.com/tianlinzz/agent-cli-gateway/agent/claudecode"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() {
	_ = runtime.Register("claude-code", New)
}

// Options is trusted deployment configuration for the Claude adapter.
type Options struct {
	Command            string
	Env                []string
	WorkDir            string
	Model              string
	ReasoningEffort    string
	Mode               string
	Permission         string
	SystemPrompt       string
	AppendSystemPrompt string
	AllowedTools       []string
	DisallowedTools    []string
	MaxContextTokens   int
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

// New constructs the registered adapter from the typed deployment config
// handed down by the worker boundary (O-F08: no environment-variable
// roundtrip).
func New(_ context.Context, _ string, cfg runtime.AdapterConfig) (runtime.AgentAdapter, error) {
	return NewAdapter(optionsFromConfig(cfg))
}

// optionsFromConfig maps the typed worker boundary config onto adapter
// options. Deployment values win; unset fields keep adapter defaults.
func optionsFromConfig(cfg runtime.AdapterConfig) Options {
	return Options{
		Command:            cfg.Execution.Command,
		Env:                envSlice(cfg.Execution.Env),
		WorkDir:            cfg.WorkspaceDir,
		Model:              cfg.Execution.DefaultModel,
		Mode:               "default",
		Permission:         cfg.Execution.Permission,
		InjectSystemPrompt: cfg.Execution.InjectSystemPrompt,
	}
}

// NewAdapter constructs an adapter from explicit trusted options.
func NewAdapter(opts Options) (*Adapter, error) {
	return newAdapter(opts, func(ctx context.Context, options native.Options) (nativeSession, error) {
		return native.Start(ctx, options)
	}), nil
}

func newAdapter(opts Options, start starter) *Adapter {
	if strings.TrimSpace(opts.Command) == "" {
		opts.Command = "claude"
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		opts.WorkDir = "/workspace"
	}
	if strings.TrimSpace(opts.Permission) == "" {
		opts.Permission = "auto"
	}
	return &Adapter{opts: opts, start: start}
}

// Describe returns the public Claude Code model descriptor.
func (a *Adapter) Describe(context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{
		ModelID:       "claude-code",
		DisplayName:   "Claude Code",
		Description:   "Anthropic Claude Code CLI via stream-json (persistent process)",
		LifecycleMode: runtime.LifecyclePersistentProcess,
		Capabilities: runtime.Capabilities{
			Streaming: true, ToolCalls: true, Reasoning: true,
			Permission: true, Resume: true, MultiTurn: true,
		},
	}, nil
}

// Start converts runtime metadata and trusted config into native options.
func (a *Adapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	resumeID := strings.TrimSpace(req.Metadata["claude_session_id"])
	if resumeID == "" {
		resumeID = strings.TrimSpace(req.Metadata["native_session_id"])
	}
	start := a.start
	if start == nil {
		start = func(ctx context.Context, options native.Options) (nativeSession, error) {
			return native.Start(ctx, options)
		}
	}
	session, err := start(ctx, native.Options{
		Command:            splitCommand(a.opts.Command),
		Env:                append([]string(nil), a.opts.Env...),
		WorkDir:            a.opts.WorkDir,
		Model:              a.opts.Model,
		ReasoningEffort:    a.opts.ReasoningEffort,
		ResumeID:           resumeID,
		Mode:               a.opts.Mode,
		Permission:         a.opts.Permission,
		SystemPrompt:       a.opts.SystemPrompt,
		AppendSystemPrompt: a.opts.AppendSystemPrompt,
		AllowedTools:       append([]string(nil), a.opts.AllowedTools...),
		DisallowedTools:    append([]string(nil), a.opts.DisallowedTools...),
		MaxContextTokens:   a.opts.MaxContextTokens,
	})
	if err != nil {
		return nil, err
	}
	return wrapSession(session, a.opts.InjectSystemPrompt), nil
}

func splitCommand(command string) []string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return []string{"claude"}
	}
	return parts
}

// envSlice renders a config env map as the K=V slice the native process spec
// consumes. Keys are sorted so the rendered environment is deterministic.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}
