// Package codex adapts native Codex exec sessions to the Gateway runtime.
package codex

import (
	"context"
	"os"
	"strings"

	native "github.com/tianlinzz/agent-cli-gateway/agent/codex"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() { _ = runtime.Register("codex", New) }

type Options struct {
	Command         string
	Env             []string
	WorkDir         string
	CodexHome       string
	Model           string
	ReasoningEffort string
	Mode            string
	Permission      string
	SystemPrompt    string
	AppendPrompt    string
}

type nativeSession interface {
	Send(context.Context, native.Input) error
	Events() <-chan native.Event
	Abort(context.Context) error
	Close(context.Context) error
	NativeSessionID() string
}

type starter func(context.Context, native.Options) (nativeSession, error)

type Adapter struct {
	opts  Options
	start starter
}

func New(_ context.Context, _ string) (runtime.AgentAdapter, error) {
	opts := Options{
		Command:         envOrDefault("CC_GATEWAY_CODEX_COMMAND", "codex"),
		WorkDir:         envOrDefault("GW_WORKSPACE_DIR", "/workspace"),
		CodexHome:       envOrDefault("GW_AGENT_HOME", ""),
		Model:           envOrDefault("CC_GATEWAY_CODEX_MODEL", ""),
		ReasoningEffort: envOrDefault("CC_GATEWAY_CODEX_EFFORT", ""),
		Mode:            envOrDefault("CC_GATEWAY_CODEX_MODE", "full-auto"),
		Permission:      envOrDefault("CC_GATEWAY_CODEX_PERMISSION", "auto"),
	}
	if raw := envOrDefault("CC_GATEWAY_CODEX_ENV", ""); raw != "" {
		opts.Env = splitEnv(raw)
	}
	return NewAdapter(opts)
}

func NewAdapter(opts Options) (*Adapter, error) {
	return newAdapter(opts, startNative), nil
}

func startNative(ctx context.Context, options native.Options) (nativeSession, error) {
	return native.Start(ctx, options)
}

func newAdapter(opts Options, start starter) *Adapter {
	if strings.TrimSpace(opts.Command) == "" {
		opts.Command = "codex"
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
	resumeID := strings.TrimSpace(req.Metadata["codex_thread_id"])
	if resumeID == "" {
		resumeID = strings.TrimSpace(req.Metadata["native_session_id"])
	}
	env := append([]string(nil), a.opts.Env...)
	if a.opts.CodexHome != "" {
		env = append(env, "CODEX_HOME="+a.opts.CodexHome)
	}
	start := a.start
	if start == nil {
		start = startNative
	}
	nativeSession, err := start(ctx, native.Options{
		Command: splitCommand(a.opts.Command), Env: env, WorkDir: a.opts.WorkDir,
		Model: a.opts.Model, ReasoningEffort: a.opts.ReasoningEffort, Mode: a.opts.Mode, Permission: a.opts.Permission,
		ResumeID: resumeID, SystemPrompt: a.opts.SystemPrompt, AppendSystemPrompt: a.opts.AppendPrompt,
	})
	if err != nil {
		return nil, err
	}
	return wrapSessionWithResume(nativeSession, resumeID != ""), nil
}

func splitCommand(command string) []string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return []string{"codex"}
	}
	return parts
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitEnv(raw string) []string {
	var result []string
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if value = strings.TrimSpace(value); strings.Contains(value, "=") {
			result = append(result, value)
		}
	}
	return result
}
