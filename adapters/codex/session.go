// Codex session — migrated from the upstream in-repo agent/codex/session.go
// (originally cc-connect) and adapted to the new runtime contract.
//
// This is the `exec` backend session: each Send launches a fresh `codex exec`
// subprocess (`codex exec` on the first turn, `codex exec resume <thread_id>`
// on later turns), parses the native JSONL stream on stdout, and maps it to
// canonical runtime.Events. Lifecycle mode is resume_per_turn. The app_server
// backend is deliberately NOT migrated in this first slice.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

var (
	codexSessionCloseTimeout  = 8 * time.Second
	codexSessionForceKillWait = 2 * time.Second

	// Abort retries the process-group kill because a subprocess forked by the
	// CLI concurrently with the abort (e.g. a helper spawned a microsecond after
	// the turn's last stream event) can miss the first SIGKILL. It stays alive
	// orphaned with the same pgid and keeps the stdout/stderr pipes open, which
	// would stall the read loop. Re-killing the group until the direct child is
	// reaped catches it.
	codexAbortKillRetries    = 10
	codexAbortKillRetryDelay = 25 * time.Millisecond
)

// codexSession manages a multi-turn Codex conversation on the exec backend.
type codexSession struct {
	workDir       string
	model         string
	effort        string
	mode          string
	baseURL       string   // provider base URL; passed as -c openai_base_url=<url>
	modelProvider string   // Codex model_provider name; passed as -c model_provider=<name>
	cmd           string   // CLI binary, default "codex"
	cliExtraArgs  []string // extra args from cmd, prepended before exec args
	extraEnv      []string
	preamble      string
	events        chan runtime.Event
	threadID      atomic.Value // stores string — Codex native thread_id
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	alive         atomic.Bool
	closeOnce     sync.Once
	cmdMu         sync.Mutex
	cmds          map[*exec.Cmd]struct{}
	inFlight      *exec.Cmd // current turn's subprocess, guarded by cmdMu

	pendingMsgs []string // buffered agent_message texts awaiting final flush

	contextMu   sync.RWMutex
	lastUsage   *runtime.Usage
	sessionFile string
}

func newCodexSession(ctx context.Context, cliBin string, cliExtraArgs []string, workDir, model, effort, mode, resumeID, baseURL string, extraEnv []string, modelProvider string, systemPrompt string, appendPrompt string) (*codexSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	cs := &codexSession{
		workDir:       workDir,
		model:         model,
		effort:        effort,
		mode:          mode,
		baseURL:       baseURL,
		modelProvider: modelProvider,
		cmd:           cliBin,
		cliExtraArgs:  cliExtraArgs,
		extraEnv:      extraEnv,
		preamble:      buildCodexPromptPreamble(systemPrompt, appendPrompt),
		events:        make(chan runtime.Event, 64),
		ctx:           sessionCtx,
		cancel:        cancel,
		cmds:          make(map[*exec.Cmd]struct{}),
	}
	cs.alive.Store(true)

	if resumeID != "" {
		cs.threadID.Store(resumeID)
	}

	return cs, nil
}

// Send launches a codex subprocess for one turn. If a native threadID exists
// (from a prior turn or a native resume id), uses `codex exec resume <id>`;
// otherwise `codex exec` starts a new conversation. All events the subprocess
// emits are buffered on a 64-capacity channel so nothing is dropped between
// Send and the worker's StreamEvents RPC.
func (cs *codexSession) Send(ctx context.Context, input runtime.Input) error {
	if !cs.alive.Load() {
		return fmt.Errorf("codex session is closed")
	}

	isResume := cs.CurrentSessionID() != ""
	prompt := promptFromInput(input, isResume, cs.preamble)
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("codex: empty prompt (no user message in input)")
	}

	args := cs.buildExecArgs(prompt)
	if len(cs.cliExtraArgs) > 0 {
		args = append(append([]string{}, cs.cliExtraArgs...), args...)
	}

	bin := cs.cmd
	if bin == "" {
		bin = "codex"
	}

	slog.Debug("codexSession: launching", "resume", isResume, "args", redactArgs(args))

	cmd := exec.CommandContext(cs.ctx, bin, args...)
	cmd.Dir = cs.workDir
	prepareCmdForKill(cmd)
	if len(cs.extraEnv) > 0 {
		cmd.Env = mergeEnv(os.Environ(), cs.extraEnv)
	}
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codexSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codexSession: start: %w", err)
	}
	cs.cmdMu.Lock()
	cs.inFlight = cmd
	cs.cmds[cmd] = struct{}{}
	cs.cmdMu.Unlock()

	cs.wg.Add(1)
	go cs.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

// buildExecArgs maps the session state to a `codex exec [resume]` argv.
//
// The gateway fork pins the sandbox to danger-full-access unconditionally: the
// deployment target is a container where the container is the isolation
// boundary, so codex's OS-level sandbox only blocks legitimate tooling (e.g.
// internal CLIs that need network) with no real security gain.
//
// approval_policy must stay "never": `codex exec` (this backend) has no
// approval IPC, so any other value blocks waiting for a TTY response that never
// arrives.
//
// `codex exec resume` does NOT accept `--sandbox <mode>` (only `codex exec`
// does), so resume expresses sandbox via `-c sandbox_mode="..."`.
func (cs *codexSession) buildExecArgs(prompt string) []string {
	tid := cs.CurrentSessionID()
	isResume := tid != ""

	var args []string
	if isResume {
		args = []string{"exec", "resume", "--skip-git-repo-check"}
	} else {
		args = []string{"exec", "--skip-git-repo-check"}
	}

	if isResume {
		args = append(args, "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`)
	} else {
		args = append(args, "--sandbox", "danger-full-access", "-c", `approval_policy="never"`)
	}

	if cs.model != "" {
		args = append(args, "--model", cs.model)
	}
	if cs.modelProvider != "" {
		args = append(args, "-c", fmt.Sprintf("model_provider=%q", cs.modelProvider))
	}
	if cs.baseURL != "" {
		args = append(args, "-c", fmt.Sprintf("openai_base_url=%q", cs.baseURL))
	}
	if cs.effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", cs.effort))
	}

	if isResume {
		args = append(args, tid)
		// codex exec resume does not support --cd; cmd.Dir handles cwd instead.
		// Use stdin ("-") so multiline prompts are preserved reliably on Windows.
		args = append(args, "--json", "-")
	} else {
		args = append(args, "--json", "--cd", cs.workDir, "-")
	}
	return args
}

func (cs *codexSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer cs.wg.Done()
	defer func() {
		defer cs.removeCmd(cmd)
		if err := cmd.Wait(); err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("codexSession: process failed", "error", err, "stderr", stderrMsg)
				evt := runtime.Event{Type: runtime.EventError, Error: stderrMsg}
				cs.emit(evt)
			}
		}
	}()

	if err := readJSONLines(stdout, func(line []byte) error {
		lineText := string(line)
		if lineText == "" {
			return nil
		}

		slog.Debug("codexSession: raw", "line", truncate(lineText, 500))

		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			slog.Debug("codexSession: non-JSON line", "line", lineText)
			return nil
		}

		cs.handleEvent(raw)
		return nil
	}); err != nil {
		slog.Error("codexSession: read stdout error", "error", err)
		cs.emit(runtime.Event{Type: runtime.EventError, Error: fmt.Sprintf("read stdout: %v", err)})
	}
}

// handleEvent routes native `codex exec --json` stream events to canonical
// runtime events.
func (cs *codexSession) handleEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "thread.started":
		if tid, ok := raw["thread_id"].(string); ok {
			cs.threadID.Store(tid)
			cs.contextMu.Lock()
			cs.sessionFile = ""
			cs.lastUsage = nil
			cs.contextMu.Unlock()
			slog.Debug("codexSession: thread started", "thread_id", tid)
		}

	case "turn.started":
		cs.pendingMsgs = cs.pendingMsgs[:0]
		cs.contextMu.Lock()
		cs.lastUsage = nil
		cs.contextMu.Unlock()
		slog.Debug("codexSession: turn started")

	case "item.started":
		cs.handleItemStarted(raw)

	case "item.completed":
		cs.handleItemCompleted(raw)

	case "turn.completed":
		cs.refreshContextUsageFromRollout()
		cs.flushPendingAsText()
		cs.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn", NativeSessionID: cs.CurrentSessionID()})
		if usage := cs.LastUsage(); usage != nil {
			cs.emit(runtime.Event{Type: runtime.EventUsage, Usage: usage})
		}

	case "turn.failed":
		errMsg := ""
		if errObj, ok := raw["error"].(map[string]any); ok {
			errMsg, _ = errObj["message"].(string)
		}
		if errMsg == "" {
			errMsg = "turn failed (no details)"
		}
		slog.Warn("codexSession: turn failed", "error", errMsg)
		cs.emit(runtime.Event{Type: runtime.EventError, Error: errMsg})

	case "error":
		msg, _ := raw["message"].(string)
		if strings.Contains(msg, "Reconnecting") || strings.Contains(msg, "Falling back") {
			slog.Debug("codexSession: transient error", "message", msg)
		} else {
			slog.Warn("codexSession: error event", "message", msg)
		}

	case "approval.requested":
		// The exec backend never emits this (approval_policy=never), but the
		// canonical permission path must exist: auto-approve happens at the
		// worker/config layer, not by deleting the event surface. The
		// app_server backend (deferred) surfaces real approvals here.
		if req := codexPermissionEvent(raw); req != nil {
			cs.emitPermission(*req)
		}

	default:
		slog.Debug("codexSession: unhandled event type", "type", eventType)
	}
}

// flushPendingAsText emits all buffered agent_message texts as EventText
// (final response). The canonical contract has no separate "thinking" type, so
// reasoning content and pre-tool buffered messages both ride on EventText; the
// OpenAI conversion layer decides how to surface them.
func (cs *codexSession) flushPendingAsText() {
	if cs.ctx.Err() != nil {
		return
	}
	for _, text := range cs.pendingMsgs {
		if cs.ctx.Err() != nil {
			return
		}
		cs.emit(runtime.Event{Type: runtime.EventText, Text: text})
	}
	cs.pendingMsgs = cs.pendingMsgs[:0]
}

func (cs *codexSession) handleItemStarted(raw map[string]any) {
	item, ok := raw["item"].(map[string]any)
	if !ok {
		slog.Debug("codexSession: item.started missing item field")
		return
	}
	itemType, _ := item["type"].(string)
	slog.Debug("codexSession: item.started", "item_type", itemType)

	if itemType == "agent_message" || itemType == "message" || itemType == "reasoning" {
		return
	}

	// Any non-message item is a tool (or tool-adjacent) event. Mirror the
	// upstream agent/codex behavior: flush the buffered agent_message
	// commentary FIRST, so the user sees it streamed BEFORE the tool executes,
	// and so it is not lost if the turn is aborted mid-tool (the
	// turn.completed final flush would then never run).
	cs.flushPendingAsText()

	switch itemType {
	case "command_execution":
		command, _ := item["command"].(string)
		cs.emit(runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
			Name: "Bash", Arguments: toolCallArgs("command_execution", command),
		}})
	case "function_call":
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		cs.emit(runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
			Name: name, Arguments: toolCallArgs("function_call", args),
		}})
	}
	// Other tool types (web_search etc.) have empty fields at start; their
	// EventToolUse is emitted from handleItemCompleted instead.
}

func (cs *codexSession) handleItemCompleted(raw map[string]any) {
	item, ok := raw["item"].(map[string]any)
	if !ok {
		slog.Debug("codexSession: item.completed missing item field")
		return
	}
	itemType, _ := item["type"].(string)
	slog.Debug("codexSession: item.completed", "item_type", itemType)

	switch itemType {
	case "reasoning":
		text := extractItemText(item, "summary", "summary_text")
		if text != "" {
			cs.emit(runtime.Event{Type: runtime.EventText, Text: text})
		}

	case "agent_message", "message":
		text := extractItemText(item, "content", "output_text")
		if text != "" {
			cs.pendingMsgs = append(cs.pendingMsgs, text)
		}

	case "command_execution":
		command, _ := item["command"].(string)
		status, _ := item["status"].(string)
		output, _ := item["aggregated_output"].(string)
		exitCode, _ := item["exit_code"].(float64)
		code := int(exitCode)
		success := codexToolSuccess(status, &code)

		slog.Debug("codexSession: command completed",
			"command", truncate(command, 100),
			"status", status,
			"exit_code", code,
			"output_len", len(output),
		)
		cs.emit(runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{
			Name: "Bash", Result: truncate(strings.TrimSpace(output), 500), IsError: !success,
		}})

	case "function_call":
		name, _ := item["name"].(string)
		status, _ := item["status"].(string)
		output, _ := item["output"].(string)
		success := codexToolSuccess(status, nil)
		slog.Debug("codexSession: function_call completed",
			"name", name, "status", status, "output_len", len(output),
		)
		cs.emit(runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{
			Name: name, Result: truncate(strings.TrimSpace(output), 500), IsError: !success,
		}})

	case "function_call_output":
		slog.Debug("codexSession: function_call_output")

	case "error":
		msg, _ := item["message"].(string)
		if msg != "" && !strings.Contains(msg, "Falling back") {
			slog.Warn("codexSession: item error", "message", msg)
		}

	default:
		if toolName, known := codexToolNames[itemType]; known {
			input := codexExtractToolInput(item)
			cs.emit(runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
				Name: toolName, Arguments: map[string]any{"input": input},
			}})
		} else {
			slog.Debug("codexSession: unhandled item type", "item_type", itemType)
		}
	}
}

// codexPermissionEvent maps a permission-like stream event to a canonical
// PermissionRequest. On the exec backend this path is inert (approval_policy is
// always "never"); it exists so the canonical permission event surface is
// preserved for the future app_server backend and any CLI that surfaces
// approvals.
func codexPermissionEvent(raw map[string]any) *runtime.PermissionRequest {
	eventType, _ := raw["type"].(string)
	if eventType != "approval.requested" {
		return nil
	}
	id, _ := raw["id"].(string)
	if strings.TrimSpace(id) == "" {
		if rid, ok := raw["request_id"].(string); ok && rid != "" {
			id = rid
		} else {
			id = "approval-unknown"
		}
	}
	action, _ := raw["action"].(string)
	if action == "" {
		action = "unknown"
	}
	detail, _ := raw["detail"].(string)
	return &runtime.PermissionRequest{ID: id, Action: action, Detail: detail}
}

// emit sends an event to the session's canonical stream, dropping it only if
// the session is being torn down.
func (cs *codexSession) emit(evt runtime.Event) {
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
	}
}

// emitPermission sends a canonical permission event (the event surface that
// survives even when the deployment auto-approves at the worker/config layer).
func (cs *codexSession) emitPermission(req runtime.PermissionRequest) {
	cs.emit(runtime.Event{Type: runtime.EventPermission, Permission: &req})
}

func (cs *codexSession) Events() <-chan runtime.Event {
	return cs.events
}

// CurrentSessionID returns the native Codex thread id, or "" on a fresh
// session.
func (cs *codexSession) CurrentSessionID() string {
	v, _ := cs.threadID.Load().(string)
	return v
}

// LastUsage returns the most recent turn's token usage, or nil.
func (cs *codexSession) LastUsage() *runtime.Usage {
	cs.contextMu.RLock()
	defer cs.contextMu.RUnlock()
	if cs.lastUsage == nil {
		return nil
	}
	u := *cs.lastUsage
	return &u
}

// Abort cancels the in-flight turn by killing its process group. The session
// stays alive for the next turn (resume_per_turn lifecycle).
func (cs *codexSession) Abort(ctx context.Context) error {
	if !cs.alive.Load() {
		return nil
	}
	cs.cmdMu.Lock()
	cmd := cs.inFlight
	cs.cmdMu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// Kill the whole group. A subprocess forked concurrently with the abort can
	// miss the first signal and survive orphaned holding the pipes open; keep
	// re-killing the group (same pgid) until the read loop reaps the direct
	// child, bounded by ctx/time.
	for attempt := 0; attempt < codexAbortKillRetries; attempt++ {
		if err := forceKillCmd(cmd); err != nil {
			return err
		}
		if !cs.hasCmd(cmd) {
			return nil // direct child reaped → the whole tree is gone
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(codexAbortKillRetryDelay):
		}
	}
	slog.Warn("codexSession: abort: in-flight process group still alive after retries", "pid", cmd.Process.Pid)
	return nil
}

// hasCmd reports whether cmd is still tracked (not yet reaped by the read
// loop).
func (cs *codexSession) hasCmd(cmd *exec.Cmd) bool {
	cs.cmdMu.Lock()
	defer cs.cmdMu.Unlock()
	_, ok := cs.cmds[cmd]
	return ok
}

// refreshContextUsageFromRollout reads the native codex rollout token-count
// record for the current thread id. Best effort: a single synchronous attempt,
// so an unavailable rollout only skips the turn's EventUsage.
func (cs *codexSession) refreshContextUsageFromRollout() {
	sessionID := strings.TrimSpace(cs.CurrentSessionID())
	if sessionID == "" {
		return
	}

	cs.contextMu.RLock()
	cachedPath := cs.sessionFile
	cs.contextMu.RUnlock()

	usage, path, err := loadContextUsageFromRollout(cs.extraEnv, sessionID, cachedPath)
	if err != nil {
		slog.Debug("codexSession: context usage unavailable", "thread_id", sessionID, "error", err)
		return
	}
	if usage == nil {
		return
	}
	cs.contextMu.Lock()
	cs.sessionFile = path
	cs.lastUsage = usage
	cs.contextMu.Unlock()
}

// Close tears the session down: cancel the session context, wait for the read
// loops, then escalate to a process-group kill before closing the event
// stream. ctx satisfies the runtime.Session interface; teardown bounds are the
// session's own timeouts.
func (cs *codexSession) Close(ctx context.Context) error {
	cs.alive.Store(false)
	cs.cancel()
	done := make(chan struct{})
	go func() {
		cs.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// readLoop has exited; safe to close the events channel.
		cs.closeOnce.Do(func() {
			close(cs.events)
		})
		return nil
	case <-time.After(codexSessionCloseTimeout):
		cmds := cs.activeCmds()
		slog.Warn("codexSession: graceful close timed out, killing active process groups",
			"wait", codexSessionCloseTimeout,
			"count", len(cmds))
		if err := forceKillAllCmds(cmds); err != nil {
			slog.Debug("codexSession: force kill failed", "error", err)
		}
		select {
		case <-done:
			cs.closeOnce.Do(func() {
				close(cs.events)
			})
			return nil
		case <-time.After(codexSessionForceKillWait):
			// Do not close(cs.events) here: readLoop may still be in handleEvent
			// (e.g. turn.completed -> flushPendingAsText) and would panic on send.
			slog.Warn("codexSession: force kill wait timed out, deferring events channel close until readLoop exits",
				"wait", codexSessionForceKillWait)
			go func() {
				<-done
				cs.closeOnce.Do(func() {
					close(cs.events)
				})
			}()
			return nil
		}
	}
}

func (cs *codexSession) removeCmd(cmd *exec.Cmd) {
	cs.cmdMu.Lock()
	defer cs.cmdMu.Unlock()
	delete(cs.cmds, cmd)
	if cs.inFlight == cmd {
		cs.inFlight = nil
	}
}

func (cs *codexSession) activeCmds() []*exec.Cmd {
	cs.cmdMu.Lock()
	defer cs.cmdMu.Unlock()
	cmds := make([]*exec.Cmd, 0, len(cs.cmds))
	for cmd := range cs.cmds {
		cmds = append(cmds, cmd)
	}
	return cmds
}

func forceKillAllCmds(cmds []*exec.Cmd) error {
	var errs []error
	for _, cmd := range cmds {
		if err := forceKillCmd(cmd); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
