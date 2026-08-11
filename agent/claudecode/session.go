// Package claudecode drives the native Claude Code stream-json protocol.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
	agentprotocol "github.com/tianlinzz/agent-cli-gateway/agent/protocol"
)

type activeTurn struct {
	done        chan struct{}
	abortOnce   sync.Once
	abortErr    error
	interrupted atomic.Bool
}

// Session owns one persistent Claude Code CLI process.
type Session struct {
	opts       Options
	process    *agentprocess.Process
	events     chan Event
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	stdinMu    sync.Mutex
	turnMu     sync.Mutex
	turn       *activeTurn
	pendingMu  sync.Mutex
	pending    map[string]chan controlResponse
	requestSeq atomic.Uint64
	sessionID  atomic.Value
	alive      atomic.Bool
	closing    atomic.Bool
	closeOnce  sync.Once
	promptFile string
	secrets    []string
}

// Start launches one persistent native Claude Code process.
func Start(ctx context.Context, options Options) (*Session, error) {
	opts := NormalizeOptions(options)
	if opts.Mode == "bypassPermissions" && runningAsRoot() {
		slog.Warn("claudecode: bypassPermissions is unavailable as root; using auto")
		opts.Mode = "auto"
	}

	promptFile := ""
	if strings.TrimSpace(opts.AppendSystemPrompt) != "" {
		var err error
		promptFile, err = writeAppendPromptFile(opts.AppendSystemPrompt)
		if err != nil {
			return nil, protocolError("write append system prompt: %w", err)
		}
	}

	command := append([]string(nil), opts.Command...)
	command = append(command, BuildArgs(opts, promptFile)...)
	// Claude refuses nested launches when CLAUDECODE is inherited. Replacing it
	// with an empty value is portable and equivalent to absence for the CLI.
	env := append([]string(nil), opts.Env...)
	env = append(env, "CLAUDECODE=")
	sessionCtx, cancel := context.WithCancel(ctx)
	proc, err := agentprocess.Start(sessionCtx, agentprocess.Spec{
		Command: command,
		Dir:     opts.WorkDir,
		Env:     env,
		Stdin:   true,
	})
	if err != nil {
		cancel()
		if promptFile != "" {
			_ = os.Remove(promptFile)
		}
		return nil, protocolError("start: %w", err)
	}

	session := &Session{
		opts:       opts,
		process:    proc,
		events:     make(chan Event, 64),
		ctx:        sessionCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
		promptFile: promptFile,
		secrets:    secretValues(opts.Env),
		pending:    make(map[string]chan controlResponse),
	}
	session.sessionID.Store(opts.ResumeID)
	session.alive.Store(true)
	go session.readLoop()
	if err := session.requestControl(ctx, "initialize"); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = session.Close(closeCtx)
		closeCancel()
		return nil, protocolError("initialize control protocol: %w", err)
	}
	return session, nil
}

func (s *Session) readLoop() {
	defer s.finish()

	waitDone := make(chan struct{})
	go func() {
		_ = s.process.Wait()
		close(waitDone)
		// A descendant may still hold stdout. Give buffered output a short drain
		// window, then close our read end so teardown cannot hang indefinitely.
		time.Sleep(50 * time.Millisecond)
		_ = s.process.Stdout().Close()
	}()

	decoder := agentprotocol.NewJSONLDecoder(s.process.Stdout(), 10*1024*1024)
	for {
		var raw map[string]any
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !s.closing.Load() {
				s.emit(Event{Kind: EventError, Err: protocolError("decode stream: %w", err)})
			}
			break
		}
		s.handleFrame(raw)
	}
	<-waitDone
}

func (s *Session) finish() {
	s.alive.Store(false)
	waitErr := s.process.Wait()
	if waitErr != nil && !s.closing.Load() {
		message := strings.TrimSpace(redactText(s.process.StderrString(), s.secrets))
		if message == "" {
			message = waitErr.Error()
		}
		s.emit(Event{Kind: EventError, Err: protocolError("process exited: %s", message)})
	}
	s.closeOnce.Do(func() {
		close(s.events)
		close(s.done)
	})
}

func (s *Session) handleFrame(raw map[string]any) {
	eventType, _ := raw["type"].(string)
	switch eventType {
	case "system":
		if id, _ := raw["session_id"].(string); id != "" && id != s.NativeSessionID() {
			s.sessionID.Store(id)
			s.emit(Event{Kind: EventNativeSession, NativeSessionID: id})
		}
	case "assistant":
		s.handleAssistant(raw)
	case "user":
		s.handleUser(raw)
	case "result":
		s.handleResult(raw)
	case "control_request":
		s.handleControlRequest(raw)
	case "control_response":
		s.handleControlResponse(raw)
	case "control_cancel_request":
		return
	default:
		slog.Debug("claudecode: unrecognized native event", "type", eventType)
	}
}

func (s *Session) handleAssistant(raw map[string]any) {
	message, _ := raw["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, value := range content {
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		switch blockType, _ := block["type"].(string); blockType {
		case "tool_use":
			name, _ := block["name"].(string)
			if name == "AskUserQuestion" {
				continue
			}
			id, _ := block["id"].(string)
			arguments, _ := block["input"].(map[string]any)
			s.emit(Event{Kind: EventToolUse, Tool: &ToolCall{ID: id, Name: name, Arguments: arguments}})
		case "thinking":
			// OpenAI message.content carries assistant output, not private
			// reasoning. Keep the native block inside this protocol boundary.
			continue
		case "text":
			if text, _ := block["text"].(string); text != "" {
				s.emit(Event{Kind: EventText, Text: text})
			}
		}
	}
}

func (s *Session) handleUser(raw map[string]any) {
	message, _ := raw["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, value := range content {
		block, ok := value.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			continue
		}
		isError, _ := block["is_error"].(bool)
		id, _ := block["tool_use_id"].(string)
		result := truncate(strings.TrimSpace(toolResultText(block["content"])), 500)
		s.emit(Event{Kind: EventToolResult, Tool: &ToolCall{ID: id, Result: result, IsError: isError}})
	}
}

func (s *Session) handleResult(raw map[string]any) {
	if id, _ := raw["session_id"].(string); id != "" && id != s.NativeSessionID() {
		s.sessionID.Store(id)
		s.emit(Event{Kind: EventNativeSession, NativeSessionID: id})
	}
	if isCompactionResult(raw) {
		return
	}
	input, output, _, _ := parseUsageMap(raw["usage"])
	turn := s.currentTurn()
	finishReason := "end_turn"
	if turn != nil && turn.interrupted.Load() {
		finishReason = "cancelled"
	}
	s.emit(Event{Kind: EventFinish, FinishReason: finishReason, NativeSessionID: s.NativeSessionID()})
	if total := input + output; total > 0 {
		s.emit(Event{Kind: EventUsage, Usage: &Usage{InputTokens: input, OutputTokens: output, TotalTokens: total}, NativeSessionID: s.NativeSessionID()})
	}
	s.finishTurn(turn)
}

type controlResponse struct {
	subtype string
	err     string
}

func (s *Session) handleControlResponse(raw map[string]any) {
	response, _ := raw["response"].(map[string]any)
	requestID, _ := response["request_id"].(string)
	if requestID == "" {
		return
	}
	result := controlResponse{}
	result.subtype, _ = response["subtype"].(string)
	result.err, _ = response["error"].(string)
	s.pendingMu.Lock()
	pending := s.pending[requestID]
	s.pendingMu.Unlock()
	if pending != nil {
		select {
		case pending <- result:
		default:
		}
	}
}

func parseUsageMap(value any) (input, output, cacheCreation, cacheRead int) {
	usage, _ := value.(map[string]any)
	return parseUsage(usage)
}

func (s *Session) handleControlRequest(raw map[string]any) {
	requestID, _ := raw["request_id"].(string)
	request, _ := raw["request"].(map[string]any)
	if request["subtype"] != "can_use_tool" {
		return
	}
	toolName, _ := request["tool_name"].(string)
	input, _ := request["input"].(map[string]any)
	if toolName == "AskUserQuestion" {
		s.emit(Event{Kind: EventError, Err: protocolError("AskUserQuestion is disabled in unattended gateway mode")})
		if err := s.respondPermission(requestID, false, "Interactive questions are not supported by this gateway."); err != nil {
			slog.Warn("claudecode: deny AskUserQuestion", "error", err)
		}
		return
	}

	s.emit(Event{Kind: EventPermission, Permission: &PermissionRequest{
		ID: requestID, Action: toolName, Detail: summarizeInput(toolName, input),
	}})
	allow := true
	message := ""
	switch {
	case s.opts.Mode == "dontAsk":
		allow = false
		message = "Permission mode is set to dontAsk."
	case s.opts.Mode == "acceptEdits" && !isEditTool(toolName) && s.opts.Permission == "deny":
		allow = false
		message = "The deployment permission policy denied this tool call."
	case s.opts.Permission == "deny":
		allow = false
		message = "The deployment permission policy denied this tool call."
	}
	if err := s.respondPermission(requestID, allow, message); err != nil {
		s.emit(Event{Kind: EventError, Err: err})
	}
}

func (s *Session) respondPermission(requestID string, allow bool, message string) error {
	return s.writeJSON(permissionResponse(requestID, allow, message))
}

// Send writes one user turn to persistent stdin.
func (s *Session) Send(_ context.Context, input Input) error {
	if !s.alive.Load() {
		return protocolError("session process is not running")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return protocolError("empty prompt")
	}
	s.turnMu.Lock()
	if s.turn != nil {
		s.turnMu.Unlock()
		return protocolError("turn already active")
	}
	turn := &activeTurn{done: make(chan struct{})}
	s.turn = turn
	s.turnMu.Unlock()
	if err := s.writeJSON(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": input.Prompt},
	}); err != nil {
		s.clearTurn(turn)
		return err
	}
	return nil
}

func (s *Session) currentTurn() *activeTurn {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return s.turn
}

func (s *Session) finishTurn(turn *activeTurn) {
	if turn == nil {
		return
	}
	s.turnMu.Lock()
	if s.turn == turn {
		s.turn = nil
		close(turn.done)
	}
	s.turnMu.Unlock()
}

func (s *Session) clearTurn(turn *activeTurn) {
	s.turnMu.Lock()
	if s.turn == turn {
		s.turn = nil
		close(turn.done)
	}
	s.turnMu.Unlock()
}

func (s *Session) writeJSON(value any) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return protocolError("marshal stdin frame: %w", err)
	}
	if _, err := s.process.Stdin().Write(append(data, '\n')); err != nil {
		return protocolError("write stdin: %w", err)
	}
	return nil
}

// Events returns the native event stream. Session owns and closes the channel.
func (s *Session) Events() <-chan Event { return s.events }

// NativeSessionID returns the resumable Claude conversation identifier.
func (s *Session) NativeSessionID() string {
	value, _ := s.sessionID.Load().(string)
	return value
}

// Alive reports whether the persistent native process is still running.
func (s *Session) Alive() bool { return s.alive.Load() }

func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

// Abort interrupts only the active turn through Claude's bidirectional
// control protocol. It waits for the turn's result so a later Send cannot race
// with stale events, while the persistent native process remains alive.
func (s *Session) Abort(ctx context.Context) error {
	if !s.alive.Load() {
		return nil
	}
	turn := s.currentTurn()
	if turn == nil {
		return nil
	}
	turn.abortOnce.Do(func() { turn.abortErr = s.interruptTurn(ctx, turn) })
	return turn.abortErr
}

func (s *Session) interruptTurn(ctx context.Context, turn *activeTurn) error {
	select {
	case <-turn.done:
		return nil
	default:
	}
	turn.interrupted.Store(true)
	if err := s.requestControl(ctx, "interrupt"); err != nil {
		turn.interrupted.Store(false)
		select {
		case <-turn.done:
			return nil
		default:
		}
		return s.interruptEscalation(ctx, err)
	}
	select {
	case <-turn.done:
		return nil
	case <-ctx.Done():
		return s.interruptEscalation(ctx, protocolError("interrupt settle: %w", ctx.Err()))
	}
}

func (s *Session) interruptEscalation(ctx context.Context, cause error) error {
	if err := s.process.ForceKill(); err != nil {
		return protocolError("interrupt failed (%v), force kill: %w", cause, err)
	}
	select {
	case <-s.done:
		return cause
	case <-ctx.Done():
		return protocolError("interrupt failed (%v), reap: %w", cause, ctx.Err())
	}
}

func (s *Session) requestControl(ctx context.Context, subtype string) error {
	requestID := fmt.Sprintf("gateway-%d", s.requestSeq.Add(1))
	response := make(chan controlResponse, 1)
	s.pendingMu.Lock()
	s.pending[requestID] = response
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
	}()
	if err := s.writeJSON(map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    map[string]any{"subtype": subtype},
	}); err != nil {
		return protocolError("%s request: %w", subtype, err)
	}
	select {
	case result := <-response:
		if result.subtype != "success" {
			if result.err == "" {
				result.err = result.subtype
			}
			return protocolError("%s rejected: %s", subtype, result.err)
		}
		return nil
	case <-ctx.Done():
		return protocolError("%s response: %w", subtype, ctx.Err())
	case <-s.done:
		return protocolError("%s response: process exited", subtype)
	}
}

// Close closes stdin, waits for a clean exit, then escalates to process-group
// termination. It is idempotent.
func (s *Session) Close(ctx context.Context) error {
	s.closing.Store(true)
	defer func() {
		if s.promptFile != "" {
			_ = os.Remove(s.promptFile)
		}
	}()
	if !s.alive.Load() {
		return nil
	}
	s.stdinMu.Lock()
	err := s.process.CloseStdin()
	s.stdinMu.Unlock()
	if err != nil {
		slog.Warn("claudecode: close stdin", "error", err)
	}
	select {
	case <-s.done:
		return nil
	case <-time.After(s.opts.CloseTimeout):
	}
	if err := s.process.SignalGraceful(); err != nil {
		slog.Warn("claudecode: graceful process signal", "error", err)
	}
	select {
	case <-s.done:
		return nil
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return protocolError("close wait: %w", ctx.Err())
	}
	if err := s.process.ForceKill(); err != nil {
		return protocolError("close force kill: %w", err)
	}
	select {
	case <-s.done:
		s.cancel()
		return nil
	case <-ctx.Done():
		return protocolError("close reap: %w", ctx.Err())
	}
}
