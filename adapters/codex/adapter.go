// Codex adapter — migrated from the upstream in-repo agent/codex/codex.go
// (originally cc-connect) and adapted to the new runtime contract.
//
// It implements runtime.AgentAdapter for the Codex `exec` backend: CLI launch,
// native stream parsing, native thread-id resume, usage, abort and process-group
// teardown live in session.go/protocol.go; this file owns the adapter surface
// (registration, descriptor, Start) and the small arg/env normalizers.
//
// Deliberately NOT migrated (out of scope / deleted in task 7): provider
// switching, workspace listing, session history, skill dirs, live config,
// MCP config, the quota HTTP usage endpoint (net/http), model catalog fetching
// (net/http), and the app_server backend (deferred).
package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() {
	_ = runtime.Register("codex", New)
}

// Options configures a CodexAdapter. The registered factory (New) builds these
// from process environment; the worker main and tests can build a
// CodexAdapter directly from an explicit Options.
type Options struct {
	// Command is the codex CLI binary (may include extra args, e.g.
	// "my-codex --no-color"). Empty defaults to "codex".
	Command string
	// Env adds extra KEY=VALUE env pairs for every spawned CLI process.
	Env []string
	// WorkDir is the workspace directory the CLI runs in. Empty resolves to
	// GW_WORKSPACE_DIR (default "/workspace", the nsjail mount point).
	WorkDir string
	// CodexHome is the per-session agent home used as CODEX_HOME. Empty
	// resolves to GW_AGENT_HOME.
	CodexHome string
	// Model is the default model passed as --model. Empty leaves the CLI
	// default.
	Model string
	// ReasoningEffort is passed as -c model_reasoning_effort=...
	ReasoningEffort string
	// Mode is the approval mode; on the exec backend it is informational
	// (sandbox is pinned to danger-full-access, approval_policy=never).
	Mode string
	// Backend is "exec" (supported) or "app_server" (deferred; Start errors).
	Backend string
	// Permission is the deployment permission mode ("auto"/"ask"/"deny").
	// Auto-approve happens at the worker/config layer; the adapter still keeps
	// the canonical permission event path.
	Permission string
	// SystemPrompt / AppendPrompt are project prompts prepended on fresh turns.
	SystemPrompt string
	AppendPrompt string
}

// CodexAdapter implements runtime.AgentAdapter for the Codex exec backend.
type CodexAdapter struct {
	opts Options
}

// New is the registry factory: it derives Options from the process
// environment the worker process sets for this adapter and verifies the CLI
// binary is executable.
func New(ctx context.Context, name string) (runtime.AgentAdapter, error) {
	opts := Options{
		Command:         envOrDefault("CC_GATEWAY_CODEX_COMMAND", "codex"),
		WorkDir:         envOrDefault("GW_WORKSPACE_DIR", "/workspace"),
		CodexHome:       envOrDefault("GW_AGENT_HOME", ""),
		Model:           envOrDefault("CC_GATEWAY_CODEX_MODEL", ""),
		ReasoningEffort: envOrDefault("CC_GATEWAY_CODEX_EFFORT", ""),
		Mode:            envOrDefault("CC_GATEWAY_CODEX_MODE", "full-auto"),
		Backend:         envOrDefault("CC_GATEWAY_CODEX_BACKEND", "exec"),
		Permission:      envOrDefault("CC_GATEWAY_CODEX_PERMISSION", "auto"),
	}
	if raw := envOrDefault("CC_GATEWAY_CODEX_ENV", ""); raw != "" {
		opts.Env = splitEnv(raw)
	}
	if err := ensureCLIExecutable(opts); err != nil {
		return nil, err
	}
	return NewAdapter(opts)
}

// NewAdapter builds a CodexAdapter from explicit options. It does not check
// PATH (that is the factory's job); it only normalizes mode/backend/effort.
func NewAdapter(opts Options) (*CodexAdapter, error) {
	opts.Mode = normalizeMode(opts.Mode)
	opts.Backend = normalizeBackend(opts.Backend)
	opts.ReasoningEffort = normalizeReasoningEffort(opts.ReasoningEffort)
	if strings.TrimSpace(opts.Command) == "" {
		opts.Command = "codex"
	}
	return &CodexAdapter{opts: opts}, nil
}

// Describe returns the static descriptor: model id "codex", resume_per_turn
// lifecycle (each turn runs `codex exec` fresh and resumes via the native
// thread id), and the full capability set.
func (a *CodexAdapter) Describe(ctx context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{
		ModelID:       "codex",
		DisplayName:   "Codex",
		Description:   "OpenAI Codex CLI via codex exec (resume per turn)",
		LifecycleMode: runtime.LifecycleResumePerTurn,
		Capabilities: runtime.Capabilities{
			Streaming:  true,
			ToolCalls:  true,
			Reasoning:  true,
			Permission: true,
			Resume:     true,
			MultiTurn:  true,
		},
	}, nil
}

// Start begins a session. The CLI process is launched by the worker process
// that hosts this adapter; the workspace directory is the nsjail mount point
// (GW_WORKSPACE_DIR), never a client-supplied absolute path. A native resume
// id may be passed through StartRequest.Metadata["codex_thread_id"].
func (a *CodexAdapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	if a.opts.Backend == "app_server" {
		return nil, fmt.Errorf("codex: app_server backend is not supported yet in this adapter; use the exec backend")
	}

	workDir := a.opts.WorkDir
	if workDir == "" {
		workDir = envOrDefault("GW_WORKSPACE_DIR", "/workspace")
	}

	codexHome := a.opts.CodexHome
	if codexHome == "" {
		codexHome = envOrDefault("GW_AGENT_HOME", "")
	}

	env := append([]string(nil), a.opts.Env...)
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}

	resumeID := strings.TrimSpace(req.Metadata["codex_thread_id"])

	return newCodexSession(ctx, a.opts.Command, nil, workDir, a.opts.Model,
		a.opts.ReasoningEffort, a.opts.Mode, resumeID, "", env, "",
		a.opts.SystemPrompt, a.opts.AppendPrompt)
}

// ---------------------------------------------------------------------------
// Normalizers (ported from the upstream agent).
// ---------------------------------------------------------------------------

func normalizeBackend(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "app-server", "app_server", "appserver", "ws":
		return "app_server"
	default:
		return "exec"
	}
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

func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ""
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

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// splitEnv splits a "K=V,K2=V2" (or "K=V K2=V2") string into KEY=VALUE pairs.
func splitEnv(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part != "" && strings.Contains(part, "=") {
			out = append(out, part)
		}
	}
	return out
}

// ensureCLIExecutable verifies the configured binary resolves and is
// executable. It is exported so the worker/main can fail-closed at startup;
// the registry factory already calls it.
func ensureCLIExecutable(opts Options) error {
	cmd, _ := splitCommand(opts.Command)
	if _, err := exec.LookPath(cmd); err != nil {
		return fmt.Errorf("codex: %q CLI not found in PATH, install with: npm install -g @openai/codex", cmd)
	}
	return nil
}

func splitCommand(cmd string) (bin string, args []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "codex", nil
	}
	return parts[0], parts[1:]
}
