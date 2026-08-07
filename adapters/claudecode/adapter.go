// Claude Code adapter — migrated from the upstream in-repo agent/claudecode/
// claudecode.go (originally cc-connect) and adapted to the new runtime
// contract.
//
// It implements runtime.AgentAdapter for the Claude Code CLI: one persistent
// process driven via `--input-format stream-json` (a PERSISTENT process
// receiving multiple turns), with native session resume via `--resume
// <session_id>`. The session logic (stream-json parsing, event mapping,
// permission, usage, abort/teardown) lives in session.go/protocol.go; this
// file owns the adapter surface (registration, descriptor, Start) and the
// env normalizers.
//
// Deliberately NOT migrated (out of scope / deleted in task 7): provider
// switching and the thinking-rewrite proxy, workspace listing, session
// history, skill dirs, command dirs, memory files, live config, MCP config,
// run_as_user OS isolation, the model-catalog HTTP endpoint (net/http),
// claude-mem/Stop-hook plumbing and the shared cc-connect system prompt file
// machinery.
package claudecode

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() {
	_ = runtime.Register("claude-code", New)
}

// Options configures a ClaudeCodeAdapter. The registered factory (New) builds
// these from process environment; tests can build a ClaudeCodeAdapter directly
// from an explicit Options.
type Options struct {
	// Command is the claude CLI binary (may include extra args). Empty
	// defaults to "claude".
	Command string
	// Env adds extra KEY=VALUE env pairs for every spawned CLI process.
	Env []string
	// WorkDir is the workspace directory the CLI runs in. Empty resolves to
	// GW_WORKSPACE_DIR (default "/workspace", the nsjail mount point).
	WorkDir string
	// Model is the default model passed as --model. Empty leaves the CLI
	// default.
	Model string
	// ReasoningEffort is passed as --effort.
	ReasoningEffort string
	// Mode is the Claude permission mode ("default", "acceptEdits", "plan",
	// "auto", "bypassPermissions", "dontAsk").
	Mode string
	// Permission is the deployment permission mode ("auto"/"ask"/"deny").
	// Phase-1 auto-approve happens at the adapter level inside the controlled
	// workspace; the canonical permission event is always emitted.
	Permission string
	// SystemPrompt replaces Claude's default system prompt (--system-prompt).
	SystemPrompt string
	// AppendSystemPrompt is appended to the system prompt via a temp file
	// (--append-system-prompt-file).
	AppendSystemPrompt string
	// AllowedTools / DisallowedTools are passed as --allowedTools/--disallowedTools.
	AllowedTools    []string
	DisallowedTools []string
	// MaxContextTokens is passed as --max-context-tokens when > 0.
	MaxContextTokens int
}

// ClaudeCodeAdapter implements runtime.AgentAdapter for the Claude Code CLI.
type ClaudeCodeAdapter struct {
	opts Options
}

// New is the registry factory: it derives Options from the process
// environment the worker process sets for this adapter and verifies the CLI
// binary is executable.
func New(ctx context.Context, name string) (runtime.AgentAdapter, error) {
	opts := Options{
		Command:            envOrDefault("CC_GATEWAY_CLAUDE_COMMAND", "claude"),
		WorkDir:            envOrDefault("GW_WORKSPACE_DIR", "/workspace"),
		Model:              envOrDefault("CC_GATEWAY_CLAUDE_MODEL", ""),
		ReasoningEffort:    envOrDefault("CC_GATEWAY_CLAUDE_EFFORT", ""),
		Mode:               envOrDefault("CC_GATEWAY_CLAUDE_MODE", "default"),
		Permission:         envOrDefault("CC_GATEWAY_CLAUDE_PERMISSION", "auto"),
		SystemPrompt:       envOrDefault("CC_GATEWAY_CLAUDE_SYSTEM_PROMPT", ""),
		AppendSystemPrompt: envOrDefault("CC_GATEWAY_CLAUDE_APPEND_PROMPT", ""),
	}
	if raw := envOrDefault("CC_GATEWAY_CLAUDE_ENV", ""); raw != "" {
		opts.Env = splitEnv(raw)
	}
	if err := ensureCLIExecutable(opts); err != nil {
		return nil, err
	}
	return NewAdapter(opts)
}

// NewAdapter builds a ClaudeCodeAdapter from explicit options. It does not
// check PATH (that is the factory's job); it only normalizes mode/effort.
func NewAdapter(opts Options) (*ClaudeCodeAdapter, error) {
	opts.Mode = normalizePermissionMode(opts.Mode)
	opts.ReasoningEffort = normalizeEffort(opts.ReasoningEffort)
	if strings.TrimSpace(opts.Command) == "" {
		opts.Command = "claude"
	}
	return &ClaudeCodeAdapter{opts: opts}, nil
}

// Describe returns the static descriptor: model id "claude-code",
// persistent_process lifecycle (one CLI process receives multiple turns via
// stream-json stdin), and the full capability set including permission.
func (a *ClaudeCodeAdapter) Describe(ctx context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{
		ModelID:       "claude-code",
		DisplayName:   "Claude Code",
		Description:   "Anthropic Claude Code CLI via stream-json (persistent process)",
		LifecycleMode: runtime.LifecyclePersistentProcess,
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

// Start begins a session by launching the persistent Claude Code process.
// The CLI is launched by the worker process that hosts this adapter; the
// workspace directory is the nsjail mount point (GW_WORKSPACE_DIR), never a
// client-supplied absolute path. A native resume id may be passed through
// StartRequest.Metadata["claude_session_id"].
func (a *ClaudeCodeAdapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	workDir := a.opts.WorkDir
	if workDir == "" {
		workDir = envOrDefault("GW_WORKSPACE_DIR", "/workspace")
	}

	env := append([]string(nil), a.opts.Env...)

	bin, extraArgs := splitCommand(a.opts.Command)
	resumeID := strings.TrimSpace(req.Metadata["claude_session_id"])

	cs, err := newClaudeSession(ctx, workDir, bin, extraArgs, a.opts.Model,
		a.opts.ReasoningEffort, resumeID, a.opts.Mode, a.opts.SystemPrompt,
		a.opts.AppendSystemPrompt, a.opts.AllowedTools, a.opts.DisallowedTools,
		nil, env, a.opts.MaxContextTokens, "")
	if err != nil {
		return nil, err
	}
	// Deployment permission policy ("auto"/"ask"/"deny"). Phase 1 auto-approves
	// inside the controlled workspace; "deny" responds deny. The canonical
	// EventPermission is emitted regardless before the response.
	cs.permission = a.opts.Permission
	return cs, nil
}

// ensureCLIExecutable verifies the configured binary resolves and is
// executable. It is exported so the worker/main can fail-closed at startup;
// the registry factory already calls it.
func ensureCLIExecutable(opts Options) error {
	cmd, _ := splitCommand(opts.Command)
	if _, err := exec.LookPath(cmd); err != nil {
		return fmt.Errorf("claudecode: %q CLI not found in PATH, please install it first", cmd)
	}
	return nil
}
