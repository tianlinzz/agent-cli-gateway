// Package codex adapts native Codex exec sessions to the Gateway runtime.
package codex

import (
	"context"
	"sort"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/adapters/internal/bridge"
	native "github.com/tianlinzz/agent-cli-gateway/agent/codex"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() { _ = runtime.Register("codex", New) }

type Options struct {
	// Command is the CLI command in argv form (executable first), passed to
	// exec verbatim — never through a shell or whitespace re-tokenization.
	Command         []string
	Env             []string
	WorkDir         string
	CodexHome       string
	Model           string
	ReasoningEffort string
	Mode            string
	Permission      string
	SystemPrompt    string
	AppendPrompt    string
	// InjectSystemPrompt forwards caller-supplied system role messages into the
	// native prompt at each turn.
	InjectSystemPrompt bool
}

type starter func(context.Context, native.Options) (bridge.NativeSession, error)

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
		CodexHome:          cfg.AgentHome,
		Model:              cfg.Execution.DefaultModel,
		Mode:               "full-auto",
		Permission:         cfg.Execution.Permission,
		InjectSystemPrompt: cfg.Execution.InjectSystemPrompt,
	}
}

// NewAdapter constructs an adapter from explicit trusted options.
func NewAdapter(opts Options) (*Adapter, error) {
	return newAdapter(opts, startNative), nil
}

func startNative(ctx context.Context, options native.Options) (bridge.NativeSession, error) {
	return native.Start(ctx, options)
}

func newAdapter(opts Options, start starter) *Adapter {
	if len(opts.Command) == 0 {
		opts.Command = []string{"codex"}
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		opts.WorkDir = "/workspace"
	}
	if strings.TrimSpace(opts.Permission) == "" {
		opts.Permission = "auto"
	}
	return &Adapter{opts: opts, start: start}
}

func (a *Adapter) Describe(context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{
		ModelID: "codex", DisplayName: "Codex",
		Description:   "OpenAI Codex CLI via persistent app-server",
		LifecycleMode: runtime.LifecyclePersistentProcess,
		Capabilities:  runtime.Capabilities{Streaming: true, ToolCalls: true, Reasoning: true, Permission: true, Resume: true, MultiTurn: true},
	}, nil
}

func (a *Adapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	resumeID := bridge.ResumeID(req.Metadata)
	env := append([]string(nil), a.opts.Env...)
	if a.opts.CodexHome != "" {
		env = append(env, "CODEX_HOME="+a.opts.CodexHome)
	}
	start := a.start
	if start == nil {
		start = startNative
	}
	nativeSession, err := start(ctx, native.Options{
		Command: append([]string(nil), a.opts.Command...), Env: env, WorkDir: a.opts.WorkDir,
		Model: a.opts.Model, ReasoningEffort: a.opts.ReasoningEffort, Mode: a.opts.Mode, Permission: a.opts.Permission,
		ResumeID: resumeID, SystemPrompt: a.opts.SystemPrompt, AppendSystemPrompt: a.opts.AppendPrompt,
	})
	if err != nil {
		return nil, err
	}
	return bridge.Wrap(nativeSession, bridge.WrapOptions{Adapter: "codex", InjectSystemPrompt: a.opts.InjectSystemPrompt}), nil
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
