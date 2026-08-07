// Claude Code session — migrated from the upstream in-repo
// agent/claudecode/session.go (originally cc-connect) and adapted to the new
// runtime contract.
//
// Lifecycle mode is persistent_process: ONE Claude Code CLI process is
// launched at session Start and driven bidirectionally via stream-json on
// stdin/stdout (`--input-format stream-json` + `--permission-prompt-tool
// stdio`). Each Send writes a user turn to stdin; resume happens via
// `--resume <session_id>` at launch. Native stream events (system, assistant,
// user/tool_result, result, control_request) are mapped to canonical runtime
// events, including the permission event surface: can_use_tool control
// requests emit a canonical EventPermission and then auto-respond (phase-1
// auto-approve) so the CLI never stalls.
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// claudeSession manages a long-running Claude Code process using
// --input-format stream-json and --permission-prompt-tool stdio.
type claudeSession struct {
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	stdinMu         sync.Mutex
	events          chan runtime.Event
	sessionID       atomic.Value // stores string
	permissionMode  atomic.Value // stores string
	autoApprove     atomic.Bool
	acceptEditsOnly atomic.Bool
	dontAsk         atomic.Bool
	workDir         string
	// permission is the deployment permission mode ("auto"/"ask"/"deny").
	// Phase 1 auto-approves inside the controlled workspace; "deny" responds
	// deny. The canonical EventPermission is emitted before any response.
	permission string
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	alive      atomic.Bool

	// activeModel stores the model id reported by the CLI's init event.
	activeModel atomic.Value // stores string

	// usageMu guards lastUsage. Populated from the most recent assistant and
	// result events.
	usageMu   sync.Mutex
	lastUsage *runtime.Usage

	// gracefulStopTimeout is how long Close() waits for a clean exit
	// (stdin close → process exit) before escalating to SIGTERM and then
	// SIGKILL.
	gracefulStopTimeout time.Duration

	// promptFilePath is the per-spawn temp file holding the
	// --append-system-prompt-file content. Removed on Close.
	promptFilePath string

	closeOnce sync.Once
}

// newClaudeSession launches the persistent Claude Code process. The process
// is started here (not on the first Send) because the persistent-process
// lifecycle owns one CLI for the whole session.
func newClaudeSession(ctx context.Context, workDir, cliBin string, cliExtraArgs []string, model, effort, sessionID, mode, systemPrompt, appendSystemPrompt string, allowedTools, disallowedTools, pluginDirs []string, extraEnv []string, maxContextTokens int, ccDataDir string) (*claudeSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	// Claude Code rejects bypassPermissions when running as root.
	// Downgrade to "auto" which auto-approves internally.
	if mode == "bypassPermissions" && os.Geteuid() == 0 {
		slog.Warn("claudeSession: bypassPermissions not allowed under root, downgrading to auto mode")
		mode = "auto"
	}

	// The append-system-prompt content is passed via a file (not a flag value)
	// to avoid the Windows 8192-byte command-line limit when prompts grow.
	var promptFilePath string
	if strings.TrimSpace(appendSystemPrompt) != "" {
		path, err := writeTempAppendPromptFile(ccDataDir, appendSystemPrompt)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("claudeSession: write append prompt file: %w", err)
		}
		promptFilePath = path
	}

	innerArgs := buildClaudeArgs(claudeLaunchParams{
		model:            model,
		effort:           effort,
		mode:             mode,
		sessionID:        sessionID,
		systemPrompt:     systemPrompt,
		appendPromptFile: promptFilePath,
		allowedTools:     allowedTools,
		disallowedTools:  disallowedTools,
		pluginDirs:       pluginDirs,
		maxContextTokens: maxContextTokens,
	})

	allArgs := append([]string(nil), cliExtraArgs...)
	allArgs = append(allArgs, innerArgs...)

	bin := cliBin
	if bin == "" {
		bin = "claude"
	}
	slog.Debug("claudeSession: starting", "args", redactArgs(allArgs), "dir", workDir, "mode", mode)

	cmd := exec.CommandContext(sessionCtx, bin, allArgs...)
	cmd.Dir = workDir
	prepareCmdForKill(cmd)
	env := filterEnv(os.Environ(), "CLAUDECODE")
	if len(extraEnv) > 0 {
		env = mergeEnv(env, extraEnv)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		if promptFilePath != "" {
			_ = os.Remove(promptFilePath)
		}
		return nil, fmt.Errorf("claudeSession: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		if promptFilePath != "" {
			_ = os.Remove(promptFilePath)
		}
		return nil, fmt.Errorf("claudeSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		if promptFilePath != "" {
			_ = os.Remove(promptFilePath)
		}
		cancel()
		return nil, fmt.Errorf("claudeSession: start: %w", err)
	}

	cs := &claudeSession{
		cmd:                 cmd,
		stdin:               stdin,
		events:              make(chan runtime.Event, 64),
		workDir:             workDir,
		ctx:                 sessionCtx,
		cancel:              cancel,
		done:                make(chan struct{}),
		gracefulStopTimeout: 120 * time.Second,
		promptFilePath:      promptFilePath,
	}
	cs.setPermissionMode(mode)
	cs.sessionID.Store(sessionID)
	cs.alive.Store(true)

	go cs.readLoop(stdout, &stderrBuf)

	return cs, nil
}

func (cs *claudeSession) readLoop(stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	waitErrCh, waitDone := cs.startReadLoopWait(stdout)
	defer cs.finishReadLoop(waitErrCh, stderrBuf)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		cs.handleReadLoopLine(scanner.Text())
	}

	cs.handleReadLoopScanErr(scanner.Err(), waitDone)
}

func (cs *claudeSession) startReadLoopWait(stdout io.ReadCloser) (<-chan error, <-chan struct{}) {
	waitErrCh := make(chan error, 1)
	waitDone := make(chan struct{})

	go func() {
		waitErrCh <- cs.cmd.Wait()
		close(waitDone)
	}()

	go func() {
		select {
		case <-cs.ctx.Done():
			_ = stdout.Close()
			return
		case <-waitDone:
		}

		// Grace period: give scanner a brief window to drain any data the
		// agent wrote to the pipe buffer before exiting.
		select {
		case <-cs.done:
			return
		case <-time.After(50 * time.Millisecond):
		}
		_ = stdout.Close()
	}()

	return waitErrCh, waitDone
}

func (cs *claudeSession) finishReadLoop(waitErrCh <-chan error, stderrBuf *bytes.Buffer) {
	err := <-waitErrCh

	cs.alive.Store(false)
	if err != nil {
		stderrMsg := ""
		if stderrBuf != nil {
			stderrMsg = strings.TrimSpace(stderrBuf.String())
		}
		if stderrMsg != "" {
			slog.Error("claudeSession: process failed", "error", err, "stderr", stderrMsg)
			cs.emit(runtime.Event{Type: runtime.EventError, Error: stderrMsg})
		}
	}
	cs.closeOnce.Do(func() {
		close(cs.events)
		close(cs.done)
	})
}

func (cs *claudeSession) handleReadLoopScanErr(err error, waitDone <-chan struct{}) {
	if err == nil {
		return
	}

	select {
	case <-cs.ctx.Done():
		return
	case <-waitDone:
		return
	default:
	}

	slog.Error("claudeSession: scanner error", "error", err)
	cs.emit(runtime.Event{Type: runtime.EventError, Error: fmt.Sprintf("read stdout: %v", err)})
}

func (cs *claudeSession) handleReadLoopLine(line string) {
	if line == "" {
		return
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		slog.Debug("claudeSession: non-JSON line", "line", line)
		return
	}

	eventType, _ := raw["type"].(string)
	slog.Debug("claudeSession: event", "type", eventType)

	switch eventType {
	case "system":
		cs.handleSystem(raw)
	case "assistant":
		cs.handleAssistant(raw)
	case "user":
		cs.handleUser(raw)
	case "result":
		cs.handleResult(raw)
	case "control_request":
		cs.handleControlRequest(raw)
	case "control_cancel_request":
		requestID, _ := raw["request_id"].(string)
		slog.Debug("claudeSession: permission cancelled", "request_id", requestID)
	default:
		slog.Debug("claudeSession: unrecognized event type", "type", eventType)
	}
}

func (cs *claudeSession) handleSystem(raw map[string]any) {
	if model, ok := raw["model"].(string); ok && model != "" {
		cs.activeModel.Store(model)
	}
	if sid, ok := raw["session_id"].(string); ok && sid != "" {
		cs.sessionID.Store(sid)
	}
}

func (cs *claudeSession) handleAssistant(raw map[string]any) {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return
	}

	// Capture per-sub-call usage for the canonical usage snapshot: input/cache
	// values come from the LAST assistant event (per-sub-call) so the snapshot
	// reflects the prompt size of the final inference call rather than a sum
	// that exceeds the context window. output_tokens on stream-json assistant
	// events is a placeholder, so the result event populates the final value.
	if usageRaw, ok := msg["usage"].(map[string]any); ok {
		input, _, cc, cr := parseClaudeUsage(usageRaw)
		used := input + cc + cr
		if used > 0 {
			cs.usageMu.Lock()
			prevOutput := 0
			if cs.lastUsage != nil {
				prevOutput = cs.lastUsage.OutputTokens
			}
			cs.lastUsage = &runtime.Usage{
				InputTokens:  input,
				OutputTokens: prevOutput,
				TotalTokens:  used + prevOutput,
			}
			cs.usageMu.Unlock()
		}
	}

	contentArr, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, contentItem := range contentArr {
		item, ok := contentItem.(map[string]any)
		if !ok {
			continue
		}
		contentType, _ := item["type"].(string)
		switch contentType {
		case "tool_use":
			toolName, _ := item["name"].(string)
			if toolName == "AskUserQuestion" {
				// AskUserQuestion is surfaced through the permission path, never
				// as a plain tool use.
				continue
			}
			toolInput, _ := item["input"].(map[string]any)
			cs.emit(runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
				Name: toolName, Arguments: toolInput,
			}})
		case "thinking":
			if thinking, ok := item["thinking"].(string); ok && thinking != "" {
				// The runtime contract has no separate thinking event type;
				// think blocks ride on EventText.
				cs.emit(runtime.Event{Type: runtime.EventText, Text: thinking})
			}
		case "text":
			if text, ok := item["text"].(string); ok && text != "" {
				cs.emit(runtime.Event{Type: runtime.EventText, Text: text})
			}
		}
	}
}

func (cs *claudeSession) handleUser(raw map[string]any) {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return
	}
	contentArr, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, contentItem := range contentArr {
		item, ok := contentItem.(map[string]any)
		if !ok {
			continue
		}
		contentType, _ := item["type"].(string)
		if contentType == "tool_result" {
			isError, _ := item["is_error"].(bool)
			var result string
			switch c := item["content"].(type) {
			case string:
				result = c
			case []any:
				var parts []string
				for _, elem := range c {
					if m, ok := elem.(map[string]any); ok {
						if t, _ := m["text"].(string); t != "" {
							parts = append(parts, t)
						}
					}
				}
				result = strings.Join(parts, "\n")
			}
			if isError {
				slog.Debug("claudeSession: tool error", "content", result)
			}
			cs.emit(runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{
				Result: truncateStr(strings.TrimSpace(result), 500), IsError: isError,
			}})
		}
	}
}

func (cs *claudeSession) handleResult(raw map[string]any) {
	if sid, ok := raw["session_id"].(string); ok && sid != "" {
		cs.sessionID.Store(sid)
	}

	// Compaction events arrive as `type:"result"` with `subtype:"compact"` or
	// `subtype:"compaction"`. They are NOT turn completion — the CLI is
	// mid-task and will continue streaming subsequent tool calls and assistant
	// messages after the compaction step. Treating these as terminal would
	// make the consumer return early and drop the rest of the turn (#481).
	if isCompactionResult(raw) {
		slog.Info("claudeSession: mid-turn compaction event; continuing turn", "subtype", resultSubtype(raw))
		return
	}

	// Aggregated usage across all sub-calls in this turn. The result's
	// output_tokens is the only authoritative source of total tokens generated
	// this turn (per-assistant-event output_tokens is a placeholder).
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int
	if usage, ok := raw["usage"].(map[string]any); ok {
		inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens = parseClaudeUsage(usage)
	}
	if outputTokens > 0 {
		cs.usageMu.Lock()
		if cs.lastUsage != nil {
			cs.lastUsage.OutputTokens = outputTokens
			cs.lastUsage.TotalTokens = cs.lastUsage.InputTokens + outputTokens
		} else {
			cs.lastUsage = &runtime.Usage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
				TotalTokens:  inputTokens + outputTokens,
			}
		}
		cs.usageMu.Unlock()
	}
	_ = cacheCreationTokens
	_ = cacheReadTokens

	cs.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})

	total := inputTokens + outputTokens
	if total > 0 {
		cs.emit(runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  total,
		}})
	}
}

func (cs *claudeSession) handleControlRequest(raw map[string]any) {
	requestID, _ := raw["request_id"].(string)
	request, _ := raw["request"].(map[string]any)
	if request == nil {
		return
	}
	subtype, _ := request["subtype"].(string)
	if subtype != "can_use_tool" {
		slog.Debug("claudeSession: unknown control request subtype", "subtype", subtype)
		return
	}

	toolName, _ := request["tool_name"].(string)
	input, _ := request["input"].(map[string]any)

	// The canonical permission event surface is preserved regardless of the
	// deployment's auto-approve policy: the worker/config layer decides whether
	// to approve, but the event must always exist in the stream.
	cs.emit(runtime.Event{Type: runtime.EventPermission, Permission: &runtime.PermissionRequest{
		ID:     requestID,
		Action: toolName,
		Detail: summarizeInput(toolName, input),
	}})

	if cs.autoApprove.Load() {
		slog.Debug("claudeSession: auto-approving", "request_id", requestID, "tool", toolName)
		_ = cs.respondPermission(requestID, true, "")
		return
	}
	if cs.dontAsk.Load() {
		slog.Debug("claudeSession: auto-denying", "request_id", requestID, "tool", toolName)
		_ = cs.respondPermission(requestID, false, "Permission mode is set to dontAsk.")
		return
	}
	if cs.acceptEditsOnly.Load() && isClaudeEditTool(toolName) {
		slog.Debug("claudeSession: auto-approving edit tool", "request_id", requestID, "tool", toolName)
		_ = cs.respondPermission(requestID, true, "")
		return
	}

	// Deployment-level decision. Phase 1 auto-approves inside the controlled
	// workspace; "deny" rejects. ("ask" would require a worker-side responder
	// that phase 1 does not implement, so it falls through to allow to keep the
	// CLI unblocked.)
	if cs.permission == "deny" {
		slog.Info("claudeSession: permission denied by policy", "request_id", requestID, "tool", toolName)
		_ = cs.respondPermission(requestID, false, "The deployment permission policy denied this tool call.")
		return
	}
	slog.Info("claudeSession: auto-approving (phase 1)", "request_id", requestID, "tool", toolName)
	_ = cs.respondPermission(requestID, true, "")
}

// Send writes a user turn (the last user message of the canonical input) to
// the persistent process's stdin as a stream-json user message.
func (cs *claudeSession) Send(ctx context.Context, input runtime.Input) error {
	if !cs.alive.Load() {
		return fmt.Errorf("claudeSession: session process is not running")
	}

	prompt := lastUserMessage(input)
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("claudeSession: empty prompt (no user message in input)")
	}

	return cs.writeJSON(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": prompt},
	})
}

// lastUserMessage returns the content of the last user message in the input.
// The persistent Claude process owns the conversation history natively, so
// only the newest user turn is delivered on each Send.
func lastUserMessage(in runtime.Input) string {
	var last string
	for _, m := range in.Messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			last = m.Content
		}
	}
	return last
}

// respondPermission writes a control_response to the Claude process stdin.
func (cs *claudeSession) respondPermission(requestID string, allow bool, message string) error {
	if !cs.alive.Load() {
		return fmt.Errorf("claudeSession: session process is not running")
	}

	var permResponse map[string]any
	if allow {
		// Claude Code's documented stdio control-response examples reply to an
		// "allow" with an EMPTY updatedInput (the CLI fills in the tool's input
		// itself); we deliberately do NOT echo the request's own input back the
		// way the older in-repo source (agent/claudecode/session.go
		// RespondPermission) did. Proven behavior on the phase-1 hot path.
		permResponse = map[string]any{
			"behavior":     "allow",
			"updatedInput": make(map[string]any),
		}
	} else {
		if message == "" {
			message = "The user denied this tool use. Stop and wait for the user's instructions."
		}
		permResponse = map[string]any{
			"behavior": "deny",
			"message":  message,
		}
	}

	controlResponse := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   permResponse,
		},
	}

	slog.Debug("claudeSession: permission response", "request_id", requestID, "behavior", map[bool]string{true: "allow", false: "deny"}[allow])
	return cs.writeJSON(controlResponse)
}

func (cs *claudeSession) writeJSON(v any) error {
	cs.stdinMu.Lock()
	defer cs.stdinMu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("claudeSession: marshal: %w", err)
	}
	if _, err := cs.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("claudeSession: write stdin: %w", err)
	}
	return nil
}

func (cs *claudeSession) setPermissionMode(mode string) {
	cs.permissionMode.Store(mode)
	cs.autoApprove.Store(mode == "bypassPermissions")
	cs.acceptEditsOnly.Store(mode == "acceptEdits")
	cs.dontAsk.Store(mode == "dontAsk")
}

func (cs *claudeSession) Events() <-chan runtime.Event {
	return cs.events
}

func (cs *claudeSession) CurrentSessionID() string {
	v, _ := cs.sessionID.Load().(string)
	return v
}

func (cs *claudeSession) permissionModeValue() string {
	v, _ := cs.permissionMode.Load().(string)
	return v
}

// GetContextUsage returns a snapshot of the most recent per-turn usage, or
// nil if no assistant/result event has been observed yet.
func (cs *claudeSession) GetContextUsage() *runtime.Usage {
	cs.usageMu.Lock()
	defer cs.usageMu.Unlock()
	if cs.lastUsage == nil {
		return nil
	}
	clone := *cs.lastUsage
	return &clone
}

func (cs *claudeSession) Alive() bool {
	return cs.alive.Load()
}

// emit sends an event to the session's canonical stream, dropping it only if
// the session is being torn down.
func (cs *claudeSession) emit(evt runtime.Event) {
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
	}
}

// Abort cancels the in-flight turn by terminating the persistent process
// (CommandContext kills the direct child; the process-group kill reaps any
// descendant tree, e.g. MCP bridges). The session ends — a later resume
// starts a fresh execution.
func (cs *claudeSession) Abort(ctx context.Context) error {
	if !cs.alive.Load() {
		return nil
	}
	cs.cancel()
	select {
	case <-cs.done:
		return nil
	case <-time.After(2 * time.Second):
	}
	if cs.cmd != nil {
		if err := forceKillCmd(cs.cmd); err != nil {
			slog.Warn("claudeSession: abort force kill", "error", err)
		}
	}
	select {
	case <-cs.done:
	case <-ctx.Done():
	}
	return nil
}

// Close tears the persistent process down: close stdin (Claude Code exits
// cleanly on stdin EOF, running Stop hooks), then escalate to SIGTERM and
// finally SIGKILL of the whole process group.
func (cs *claudeSession) Close(ctx context.Context) error {
	defer func() {
		if cs.promptFilePath != "" {
			_ = os.Remove(cs.promptFilePath)
		}
	}()

	// Phase 1: Close stdin to signal EOF.
	cs.stdinMu.Lock()
	if err := cs.stdin.Close(); err != nil {
		slog.Warn("claudeSession: close stdin", "error", err)
	}
	cs.stdinMu.Unlock()

	graceful := cs.gracefulStopTimeout
	if graceful <= 0 {
		graceful = 8 * time.Second // legacy fallback
	}

	select {
	case <-cs.done:
		slog.Info("claudeSession: exited cleanly after stdin close")
		return nil
	case <-time.After(graceful):
		slog.Warn("claudeSession: graceful stop timed out, sending SIGTERM", "timeout", graceful)
	}

	// Phase 2: SIGTERM the whole process group.
	if err := signalProcessGroup(cs.cmd, sigterm); err != nil {
		slog.Warn("claudeSession: signal SIGTERM", "error", err)
	}

	select {
	case <-cs.done:
		slog.Info("claudeSession: exited after SIGTERM")
		return nil
	case <-time.After(5 * time.Second):
		slog.Warn("claudeSession: SIGTERM timed out, sending SIGKILL")
	}

	// Phase 3: SIGKILL the whole process group — last resort.
	cs.cancel()
	if err := forceKillCmd(cs.cmd); err != nil {
		slog.Warn("claudeSession: force kill", "error", err)
	}
	<-cs.done
	return nil
}
