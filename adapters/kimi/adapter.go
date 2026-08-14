// Package kimi adapts native Kimi Code sessions to the Gateway runtime.
package kimi

import (
	"context"
	"sort"
	"strings"
	"time"

	native "github.com/tianlinzz/agent-cli-gateway/agent/kimi"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() { _ = runtime.Register("kimi", New) }

// Options is trusted deployment configuration for the Kimi adapter.
type Options struct {
	// Command is the CLI command in argv form (executable first), passed to
	// exec verbatim — never through a shell or whitespace re-tokenization.
	Command    []string
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
		Timeout:            cfg.Execution.TurnTimeout,
		Permission:         cfg.Execution.Permission,
		InjectSystemPrompt: cfg.Execution.InjectSystemPrompt,
	}
}

// NewAdapter constructs an adapter from explicit trusted options.
func NewAdapter(opts Options) (*Adapter, error) {
	return newAdapter(opts, startNative), nil
}

func startNative(ctx context.Context, options native.Options) (nativeSession, error) {
	return native.Start(ctx, options)
}

func newAdapter(opts Options, start starter) *Adapter {
	if len(opts.Command) == 0 {
		opts.Command = []string{"kimi"}
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		opts.WorkDir = "/workspace"
	}
	if strings.TrimSpace(opts.Permission) == "" {
		opts.Permission = "auto"
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
		Command: append([]string(nil), a.opts.Command...),
		Env:     append([]string(nil), a.opts.Env...), WorkDir: a.opts.WorkDir,
		Model: a.opts.Model, Mode: a.opts.Mode, Permission: a.opts.Permission, ResumeID: resumeID,
		Timeout: a.opts.Timeout,
	})
	if err != nil {
		return nil, err
	}
	return wrapSession(session, a.opts.InjectSystemPrompt), nil
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
