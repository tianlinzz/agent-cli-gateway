// Kimi adapter — migrated from the upstream in-repo agent/kimi/kimi.go
// (originally cc-connect) and adapted to the new runtime contract.
//
// It implements runtime.AgentAdapter for the Kimi Code CLI: CLI launch, native
// stream-json parsing, native session-id resume, abort and per-turn process
// teardown live in session.go/protocol.go; this file owns the adapter surface
// (registration, descriptor, Start) and the env normalizers.
//
// Lifecycle mode is resume_per_turn: every Send launches a fresh `kimi
// --prompt` process and resumes the native session via --resume.
//
// Deliberately NOT migrated (out of scope / deleted in task 7): provider
// switching, workspace listing, session history, skill dirs, memory files,
// PermissionModes UI, and image/file attachment staging (the runtime contract
// carries text content only).
package kimi

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func init() {
	_ = runtime.Register("kimi", New)
}

// Options configures a KimiAdapter. The registered factory (New) builds these
// from process environment; tests can build a KimiAdapter directly from an
// explicit Options.
type Options struct {
	// Command is the kimi CLI binary (may include extra args). Empty defaults
	// to "kimi".
	Command string
	// Env adds extra KEY=VALUE env pairs for every spawned CLI process.
	Env []string
	// WorkDir is the workspace directory the CLI runs in. Empty resolves to
	// GW_WORKSPACE_DIR (default "/workspace", the nsjail mount point).
	WorkDir string
	// Model is the default model passed as --model. Empty leaves the CLI
	// default.
	Model string
	// Mode is "default", "yolo", "plan", or "quiet".
	Mode string
	// Timeout bounds a single turn; zero means no per-turn timeout.
	Timeout time.Duration
	// Permission is the deployment permission mode ("auto"/"ask"/"deny").
	// The kimi CLI auto-approves tool calls in non-interactive mode, so this
	// is informational at the adapter level; the canonical permission event
	// path is preserved in the contract regardless.
	Permission string
}

// KimiAdapter implements runtime.AgentAdapter for the Kimi Code CLI.
type KimiAdapter struct {
	opts        Options
	flagSupport kimiFlagSupport // detected once via `kimi --help`
}

// New is the registry factory: it derives Options from the process
// environment the worker process sets for this adapter, probes the installed
// CLI's flag surface, and verifies the CLI binary is executable.
func New(ctx context.Context, name string) (runtime.AgentAdapter, error) {
	opts := Options{
		Command:    envOrDefault("CC_GATEWAY_KIMI_COMMAND", "kimi"),
		WorkDir:    envOrDefault("GW_WORKSPACE_DIR", "/workspace"),
		Model:      envOrDefault("CC_GATEWAY_KIMI_MODEL", ""),
		Mode:       envOrDefault("CC_GATEWAY_KIMI_MODE", "default"),
		Permission: envOrDefault("CC_GATEWAY_KIMI_PERMISSION", "auto"),
	}
	if raw := envOrDefault("CC_GATEWAY_KIMI_ENV", ""); raw != "" {
		opts.Env = splitEnv(raw)
	}
	if raw := envOrDefault("CC_GATEWAY_KIMI_TIMEOUT_SECS", ""); raw != "" {
		// The env var name promises SECONDS, so accept either a bare integer
		// (e.g. "30" = 30s) or a full duration string (e.g. "45s", "2m") for
		// flexibility. Garbage in either form falls back to the default
		// (zero = no per-turn timeout).
		if d, err := time.ParseDuration(raw); err == nil {
			opts.Timeout = d
		} else if secs, err := strconv.Atoi(raw); err == nil {
			opts.Timeout = time.Duration(secs) * time.Second
		}
	}
	if err := ensureCLIExecutable(opts); err != nil {
		return nil, err
	}
	adapter, err := NewAdapter(opts)
	if err != nil {
		return nil, err
	}
	// Probe the installed CLI's flag surface once at construction so Send can
	// adapt to CLI versions that have added or removed --print (#1456). The
	// probe has its own timeout; failures conservatively assume the modern CLI
	// surface (no --print).
	bin, _ := splitCommand(opts.Command)
	adapter.flagSupport = probeKimiFlags(ctx, bin, 5*time.Second)
	return adapter, nil
}

// NewAdapter builds a KimiAdapter from explicit options. It does not check
// PATH (that is the factory's job) nor probe the installed CLI; the probe is
// skipped so tests stay hermetic, which means the conservative modern CLI
// surface (no --print) is assumed. Callers that want version-aware probing
// should use New.
func NewAdapter(opts Options) (*KimiAdapter, error) {
	opts.Mode = normalizeMode(opts.Mode)
	if strings.TrimSpace(opts.Command) == "" {
		opts.Command = "kimi"
	}
	return &KimiAdapter{opts: opts}, nil
}

// Describe returns the static descriptor: model id "kimi", resume_per_turn
// lifecycle (each turn runs `kimi --prompt` fresh and resumes via the native
// session id), and the streaming/tool/resume capability set.
func (a *KimiAdapter) Describe(ctx context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{
		ModelID:       "kimi",
		DisplayName:   "Kimi",
		Description:   "Kimi Code CLI via kimi --prompt (resume per turn)",
		LifecycleMode: runtime.LifecycleResumePerTurn,
		Capabilities: runtime.Capabilities{
			Streaming: true,
			ToolCalls: true,
			Reasoning: true,
			Resume:    true,
			MultiTurn: true,
		},
	}, nil
}

// Start begins a session. The CLI process is launched per turn by the worker
// process that hosts this adapter; the workspace directory is the nsjail
// mount point (GW_WORKSPACE_DIR), never a client-supplied absolute path. A
// native resume id may be passed through StartRequest.Metadata["kimi_session_id"].
func (a *KimiAdapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	workDir := a.opts.WorkDir
	if workDir == "" {
		workDir = envOrDefault("GW_WORKSPACE_DIR", "/workspace")
	}

	env := append([]string(nil), a.opts.Env...)

	bin, extraArgs := splitCommand(a.opts.Command)
	resumeID := strings.TrimSpace(req.Metadata["kimi_session_id"])
	if resumeID == "" {
		resumeID = strings.TrimSpace(req.Metadata["native_session_id"])
	}

	return newKimiSession(ctx, bin, extraArgs, workDir, a.opts.Model,
		a.opts.Mode, resumeID, env, a.opts.Timeout, a.flagSupport)
}

// ensureCLIExecutable verifies the configured binary resolves and is
// executable. It is exported so the worker/main can fail-closed at startup;
// the registry factory already calls it.
func ensureCLIExecutable(opts Options) error {
	cmd, _ := splitCommand(opts.Command)
	if _, err := exec.LookPath(cmd); err != nil {
		return fmt.Errorf("kimi: %q CLI not found in PATH, install with: pip install kimi-cli", cmd)
	}
	return nil
}
