// Kimi session — migrated from the upstream in-repo agent/kimi/session.go
// (originally cc-connect) and adapted to the new runtime contract.
//
// Lifecycle mode is resume_per_turn: each Send launches a fresh `kimi
// --prompt` process (with `--print --output-format stream-json` when the
// installed binary supports it) and uses `--resume` for conversation
// continuity. The native session id is learned from the "To resume this
// session" trailer line and threaded into the next turn. All events the
// subprocess emits are buffered on a 64-capacity channel so nothing is
// dropped between Send and the worker's StreamEvents RPC.
package kimi

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

// kimiSession manages multi-turn conversations with the Kimi CLI.
type kimiSession struct {
	cmd         string
	extraArgs   []string // extra args from cmd, prepended before kimi args
	workDir     string
	model       string
	mode        string
	timeout     time.Duration
	extraEnv    []string
	flagSupport kimiFlagSupport
	events      chan runtime.Event
	sessionID   atomic.Value // stores string — Kimi session ID
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	alive       atomic.Bool

	cmdMu     sync.Mutex
	inFlight  *exec.Cmd // current turn's subprocess, guarded by cmdMu
	closeOnce sync.Once

	pendingMsgs []string // buffered assistant text messages
}

func newKimiSession(ctx context.Context, cmd string, extraArgs []string, workDir, model, mode, resumeID string, extraEnv []string, timeout time.Duration, flagSupport kimiFlagSupport) (*kimiSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	ks := &kimiSession{
		cmd:         cmd,
		extraArgs:   extraArgs,
		workDir:     workDir,
		model:       model,
		mode:        mode,
		timeout:     timeout,
		extraEnv:    extraEnv,
		flagSupport: flagSupport,
		events:      make(chan runtime.Event, 64),
		ctx:         sessionCtx,
		cancel:      cancel,
	}
	ks.alive.Store(true)

	if resumeID != "" {
		ks.sessionID.Store(resumeID)
	}

	return ks, nil
}

// buildArgs constructs the Kimi CLI argument slice for a single non-interactive
// turn. Extracted from Send() so the version-aware flag selection can be unit
// tested without launching the CLI. The `--print` flag is only emitted when
// the locally installed binary advertises it in `kimi --help`; the newer Kimi
// Code CLI dropped that flag and rejects it outright (see issue #1456).
func (ks *kimiSession) buildArgs(prompt string) []string {
	args := append([]string{}, ks.extraArgs...)
	if ks.flagSupport.Print {
		args = append(args, "--print")
	}
	args = append(args, "--output-format", "stream-json")

	switch ks.mode {
	case "plan":
		args = append(args, "--plan")
	case "quiet":
		args = append(args, "--quiet")
	}

	if sid := ks.CurrentSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	if ks.model != "" {
		args = append(args, "--model", ks.model)
	}
	if ks.workDir != "" {
		args = append(args, "--work-dir", ks.workDir)
	}

	args = append(args, "--prompt", prompt)
	return args
}

// Send launches a fresh `kimi --prompt` subprocess for one turn and streams
// its events onto the session's canonical channel.
func (ks *kimiSession) Send(ctx context.Context, input runtime.Input) error {
	if !ks.alive.Load() {
		return fmt.Errorf("kimi session is closed")
	}

	prompt := promptFromInput(input, ks.CurrentSessionID() != "")
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("kimi: empty prompt (no user message in input)")
	}

	args := ks.buildArgs(prompt)

	var cancel context.CancelFunc
	var cmdCtx context.Context
	if ks.timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ks.ctx, ks.timeout)
	} else {
		cmdCtx, cancel = context.WithCancel(ks.ctx)
	}

	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()

	slog.Debug("kimiSession: launching",
		"resume", ks.CurrentSessionID() != "",
		"supports_print", ks.flagSupport.Print,
		"args", redactArgs(args))
	cmd := exec.CommandContext(cmdCtx, ks.cmd, args...)
	cmd.WaitDelay = 1 * time.Second
	cmd.Dir = ks.workDir
	env := os.Environ()
	if len(ks.extraEnv) > 0 {
		env = mergeEnv(env, ks.extraEnv)
	}
	cmd.Env = env
	prepareCmdForKill(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("kimiSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("kimiSession: start: %w", err)
	}

	started = true
	ks.cmdMu.Lock()
	ks.inFlight = cmd
	ks.cmdMu.Unlock()

	ks.wg.Add(1)
	go func() {
		defer cancel()
		ks.readLoop(cmdCtx, cmd, stdout, &stderrBuf)
	}()

	return nil
}

func (ks *kimiSession) readLoop(ctx context.Context, cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer ks.wg.Done()
	defer func() {
		ks.cmdMu.Lock()
		if ks.inFlight == cmd {
			ks.inFlight = nil
		}
		ks.cmdMu.Unlock()
	}()

	go func() {
		<-ctx.Done()
		_ = stdout.Close()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var scanErr error
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		slog.Debug("kimiSession: raw", "line", truncate(line, 500))

		// Kimi prints a non-JSON line at the end: "To resume this session: kimi -r <id>"
		if strings.HasPrefix(line, "To resume this session:") {
			if id := extractResumeSessionID(line); id != "" {
				ks.sessionID.Store(id)
				slog.Debug("kimiSession: session id updated", "session_id", id)
			}
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			slog.Debug("kimiSession: non-JSON line", "line", line)
			continue
		}

		ks.handleEvent(raw)
	}
	scanErr = scanner.Err()

	// Wait for process exit before sending any terminal event so the consumer
	// never sees EventError after EventFinish from the same turn.
	waitErr := cmd.Wait()

	// Kimi writes "To resume this session: kimi -r <uuid>" to stderr (not stdout),
	// so the scanner above never sees it. Extract it from the captured stderr
	// buffer before emitting EventFinish so the next turn can pass --resume.
	for _, line := range strings.Split(stderrBuf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "To resume this session:") {
			if id := extractResumeSessionID(line); id != "" {
				ks.sessionID.Store(id)
				slog.Debug("kimiSession: session id from stderr", "session_id", id)
			}
			break
		}
	}

	if scanErr != nil {
		// On teardown (Abort/Close) the context watcher closes stdout, which
		// the scanner reports as "file already closed". That is not a protocol
		// error, so skip it on the cancellation path.
		if ctx.Err() == nil {
			slog.Error("kimiSession: scanner error", "error", scanErr)
			ks.emit(runtime.Event{Type: runtime.EventError, Error: fmt.Sprintf("read stdout: %v", scanErr)})
			return
		}
	}

	if waitErr != nil {
		stderrMsg := strings.TrimSpace(stderrBuf.String())
		if stderrMsg != "" {
			slog.Error("kimiSession: process failed", "error", waitErr, "stderr", stderrMsg)
			ks.emit(runtime.Event{Type: runtime.EventError, Error: stderrMsg})
			return
		}
	}

	// Flush any remaining pending messages as text and send the finish event.
	ks.flushPendingAsText()
	ks.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
}

// Kimi CLI stream-json message roles:
//   - "assistant": content (think + text), tool_calls
//   - "tool":      content (tool execution result), tool_call_id
func (ks *kimiSession) handleEvent(raw map[string]any) {
	role, _ := raw["role"].(string)

	switch role {
	case "assistant":
		ks.handleAssistant(raw)
	case "tool":
		ks.handleTool(raw)
	default:
		slog.Debug("kimiSession: unhandled role", "role", role)
	}
}

func (ks *kimiSession) handleAssistant(raw map[string]any) {
	content, _ := raw["content"].([]any)
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "think", "thinking":
			if think, ok := block["think"].(string); ok && think != "" {
				// The runtime contract has no separate thinking event type;
				// think blocks ride on EventText (same policy as the codex
				// adapter's reasoning mapping).
				ks.emit(runtime.Event{Type: runtime.EventText, Text: think})
			}
		case "text":
			if text, ok := block["text"].(string); ok && text != "" {
				ks.pendingMsgs = append(ks.pendingMsgs, text)
			}
		}
	}

	// Handle tool_calls.
	toolCalls, _ := raw["tool_calls"].([]any)
	if len(toolCalls) > 0 {
		// Flush the buffered assistant commentary BEFORE announcing the tool so
		// the user sees it streamed before the tool executes (the proven
		// upstream ordering), and so it is not lost if the turn is aborted
		// mid-tool.
		ks.flushPendingAsText()
		for _, tc := range toolCalls {
			tcMap, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			funcBlock, _ := tcMap["function"].(map[string]any)
			toolName, _ := funcBlock["name"].(string)
			args, _ := funcBlock["arguments"].(string)
			toolID, _ := tcMap["id"].(string)

			slog.Debug("kimiSession: tool_call", "tool", toolName, "id", toolID)
			ks.emit(runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
				ID:        toolID,
				Name:      toolName,
				Arguments: toolCallArgs(strings.TrimSpace(args)),
			}})
		}
	}
}

func (ks *kimiSession) handleTool(raw map[string]any) {
	toolCallID, _ := raw["tool_call_id"].(string)
	content, _ := raw["content"].([]any)
	var outputParts []string
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		if blockType == "text" {
			if text, ok := block["text"].(string); ok {
				outputParts = append(outputParts, text)
			}
		}
	}
	output := strings.Join(outputParts, "")

	if output != "" {
		slog.Debug("kimiSession: tool result", "tool_call_id", toolCallID)
		ks.emit(runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{
			ID:     toolCallID,
			Result: truncate(strings.TrimSpace(output), 500),
		}})
	}
}

func (ks *kimiSession) flushPendingAsText() {
	if len(ks.pendingMsgs) == 0 {
		return
	}
	text := strings.Join(ks.pendingMsgs, "")
	ks.pendingMsgs = ks.pendingMsgs[:0]
	if text != "" {
		ks.emit(runtime.Event{Type: runtime.EventText, Text: text})
	}
}

// emit sends an event to the session's canonical stream, dropping it only if
// the session is being torn down.
func (ks *kimiSession) emit(evt runtime.Event) {
	select {
	case ks.events <- evt:
	case <-ks.ctx.Done():
	}
}

func (ks *kimiSession) Events() <-chan runtime.Event {
	return ks.events
}

func (ks *kimiSession) CurrentSessionID() string {
	v, _ := ks.sessionID.Load().(string)
	return v
}

func (ks *kimiSession) Alive() bool {
	return ks.alive.Load()
}

// inFlightCmd returns the current turn's subprocess, or nil.
func (ks *kimiSession) inFlightCmd() *exec.Cmd {
	ks.cmdMu.Lock()
	defer ks.cmdMu.Unlock()
	return ks.inFlight
}

// Abort kills the current turn's subprocess (whole process group) so the turn
// terminates. The session stays alive for the next turn (resume_per_turn
// lifecycle).
func (ks *kimiSession) Abort(ctx context.Context) error {
	if !ks.alive.Load() {
		return nil
	}
	cmd := ks.inFlightCmd()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := forceKillCmd(cmd); err != nil {
		return err
	}
	return nil
}

// Close tears the session down: cancel the session context, wait for the read
// loops, then close the event stream. Idempotent — the second call is a no-op.
func (ks *kimiSession) Close(ctx context.Context) error {
	ks.alive.Store(false)
	ks.cancel()
	done := make(chan struct{})
	go func() {
		ks.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		ks.closeOnce.Do(func() { close(ks.events) })
	case <-time.After(8 * time.Second):
		slog.Warn("kimiSession: close timed out, abandoning wg.Wait")
	}
	return nil
}
