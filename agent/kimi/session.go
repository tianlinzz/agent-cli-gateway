// Package kimi drives one persistent native `kimi acp` process.
package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
	agentprotocol "github.com/tianlinzz/agent-cli-gateway/agent/protocol"
	"github.com/tianlinzz/agent-cli-gateway/agent/rpcsession"
)

const maxACPFrame = 10 * 1024 * 1024

type activePrompt struct {
	done      chan struct{}
	cancel    context.CancelFunc
	usage     *Usage
	tools     map[string]ToolCall
	results   map[string]bool
	aborted   bool
	completed bool
}

// Session owns one persistent Kimi ACP process. The mechanical lifecycle
// (process, monitor/teardown, event channel, abort escalation, Close) lives in
// the shared rpcsession.Core (D2); this file keeps only the ACP protocol
// specifics.
type Session struct {
	core *rpcsession.Core
	opts Options

	wg sync.WaitGroup

	mu     sync.Mutex
	active *activePrompt
}

func Start(ctx context.Context, options Options) (*Session, error) {
	opts := NormalizeOptions(options)
	s := &Session{core: rpcsession.New("kimi", "ACP", opts.CloseTimeout, protocolError), opts: opts}
	// monitor waits for in-flight prompt goroutines before the terminal-error
	// decision, so completePrompt always finalizes the active turn even when
	// the process dies mid-prompt.
	s.core.BeforeExit = s.wg.Wait
	command := append([]string(nil), opts.Command...)
	command = append(command, BuildArgs(opts, "", opts.ResumeID)...)
	if err := s.core.Launch(ctx, agentprocess.Spec{Command: command, Dir: opts.WorkDir, Env: opts.Env, Stdin: true}, maxACPFrame, s.handleReverse, s.handleNotification, isCriticalKimiNotification); err != nil {
		return nil, err
	}
	if err := s.initialize(ctx); err != nil {
		s.core.CleanupFailedStart()
		return nil, err
	}
	s.core.FinishStart()
	return s, nil
}

func (s *Session) initialize(ctx context.Context) error {
	var initialized struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := s.core.RPC().Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, &initialized); err != nil {
		return protocolError("initialize ACP: %w", err)
	}
	params := map[string]any{"cwd": s.opts.WorkDir, "mcpServers": []any{}}
	method := "session/new"
	if s.opts.ResumeID != "" {
		method = "session/resume"
		params["sessionId"] = s.opts.ResumeID
	}
	var response struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.core.RPC().Call(ctx, method, params, &response); err != nil {
		return protocolError("%s: %w", method, err)
	}
	if strings.TrimSpace(response.SessionID) == "" {
		return protocolError("%s returned an empty session id", method)
	}
	s.core.SetNativeID(response.SessionID)
	s.core.Emit(Event{Kind: EventNativeSession, NativeSessionID: response.SessionID})
	return nil
}

func (s *Session) Send(_ context.Context, input Input) error {
	if !s.core.Alive() {
		return protocolError("session is closed")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return protocolError("empty prompt")
	}
	// No native turn deadline (O-F09b): the worker boundary owns the per-turn
	// deadline and aborts this session when it fires. The turn context exists
	// only so a completed/aborted prompt releases its resources.
	turnCtx, cancel := context.WithCancel(s.core.Ctx())
	turn := &activePrompt{done: make(chan struct{}), cancel: cancel, tools: make(map[string]ToolCall), results: make(map[string]bool)}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		cancel()
		return protocolError("turn already active")
	}
	s.active = turn
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runPrompt(turnCtx, turn, prompt)
	}()
	return nil
}

func (s *Session) runPrompt(ctx context.Context, turn *activePrompt, prompt string) {
	var response struct {
		StopReason string `json:"stopReason"`
	}
	err := s.core.RPC().Call(ctx, "session/prompt", map[string]any{
		"sessionId": s.NativeSessionID(),
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	}, &response)
	if err != nil {
		s.completePrompt(turn, "", protocolError("session/prompt: %w", err))
		return
	}
	s.completePrompt(turn, response.StopReason, nil)
}

func (s *Session) completePrompt(turn *activePrompt, stopReason string, promptErr error) {
	// session/prompt's response arrives on the wire after the turn's
	// session/update notifications. The JSON-RPC reader correlates the response
	// immediately (so abort/cancel RPCs are never blocked by a stalled event
	// consumer), so drain those notifications BEFORE finalizing the turn: later
	// handlers (e.g. tool_call_update) consult s.active, which is cleared below,
	// and the consumer reads events up to the finish we emit last.
	s.core.RPC().Sync()
	s.mu.Lock()
	if turn.completed {
		s.mu.Unlock()
		return
	}
	turn.completed = true
	turn.cancel()
	if s.active == turn {
		s.active = nil
	}
	usage := turn.usage
	aborted := turn.aborted
	close(turn.done)
	s.mu.Unlock()
	if promptErr != nil && !aborted {
		s.core.Emit(Event{Kind: EventError, Err: promptErr})
		return
	}
	// Converged abort semantics (D2): an aborted turn ALWAYS ends with
	// finish("cancelled"); a prompt error on an aborted turn is the cancel's
	// side effect and is logged, never emitted as an error event.
	if promptErr != nil {
		slog.Warn("kimi: prompt failed after abort", "session_id", s.NativeSessionID(), "error", promptErr)
	}
	reason := normalizeStopReason(stopReason)
	if aborted {
		reason = "cancelled"
	}
	s.core.Emit(Event{Kind: EventFinish, FinishReason: reason, NativeSessionID: s.NativeSessionID()})
	if usage != nil {
		s.core.Emit(Event{Kind: EventUsage, Usage: usage, NativeSessionID: s.NativeSessionID()})
	}
}

// isCriticalKimiNotification routes usage updates through the never-dropped
// critical queue (converged with codex's tokenUsage classification, D2): a
// turn's usage arrives only as a session/update notification and has no
// fallback, so a stalled consumer must not be able to lose it. Everything else
// in session/update is display telemetry.
func isCriticalKimiNotification(message agentprotocol.RPCMessage) bool {
	if message.Method != "session/update" {
		return false
	}
	var probe struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	return json.Unmarshal(message.Params, &probe) == nil && probe.Update.SessionUpdate == "usage_update"
}

func (s *Session) handleNotification(message agentprotocol.RPCMessage) {
	if message.Method != "session/update" {
		return
	}
	var params map[string]any
	if json.Unmarshal(message.Params, &params) != nil || stringValue(params["sessionId"]) != s.NativeSessionID() {
		return
	}
	update := objectValue(params["update"])
	switch stringValue(update["sessionUpdate"]) {
	case "agent_message_chunk":
		content := objectValue(update["content"])
		if stringValue(content["type"]) == "text" {
			if text := stringValue(content["text"]); text != "" {
				s.core.Emit(Event{Kind: EventText, Text: text})
			}
		}
	case "agent_thought_chunk":
		content := objectValue(update["content"])
		if stringValue(content["type"]) == "text" {
			if text := stringValue(content["text"]); text != "" {
				s.core.Emit(Event{Kind: EventReasoning, Reasoning: &Reasoning{Text: text}})
			}
		}
	case "tool_call":
		s.handleToolStart(update)
	case "tool_call_update":
		s.handleToolUpdate(update)
	case "usage_update":
		// Native limitation (documented): ACP carries no turn id in
		// usage_update, so usage is attributed to the session's active turn
		// only; a late update arriving between turns is dropped.
		used := intValue(update["used"])
		if used > 0 {
			s.mu.Lock()
			if s.active != nil {
				s.active.usage = &Usage{TotalTokens: used}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Session) handleToolStart(update map[string]any) {
	tool := toolFromUpdate(update)
	if tool.ID == "" {
		return
	}
	s.mu.Lock()
	turn := s.active
	if turn == nil {
		s.mu.Unlock()
		return
	}
	if _, exists := turn.tools[tool.ID]; exists {
		s.mu.Unlock()
		return
	}
	turn.tools[tool.ID] = tool
	s.mu.Unlock()
	s.core.Emit(Event{Kind: EventToolUse, Tool: &tool})
}

func (s *Session) handleToolUpdate(update map[string]any) {
	status := strings.ToLower(stringValue(update["status"]))
	if status != "completed" && status != "failed" {
		return
	}
	tool := toolFromUpdate(update)
	if tool.ID == "" {
		return
	}
	s.mu.Lock()
	turn := s.active
	if turn == nil || turn.results[tool.ID] {
		s.mu.Unlock()
		return
	}
	turn.results[tool.ID] = true
	start, started := turn.tools[tool.ID]
	if started {
		tool.Name, tool.Arguments = start.Name, start.Arguments
	} else {
		turn.tools[tool.ID] = tool
	}
	s.mu.Unlock()
	if !started {
		copy := tool
		copy.Result = ""
		s.core.Emit(Event{Kind: EventToolUse, Tool: &copy})
	}
	tool.Result = toolContent(update["content"])
	tool.IsError = status == "failed"
	s.core.Emit(Event{Kind: EventToolResult, Tool: &tool})
}

func (s *Session) handleReverse(_ context.Context, message agentprotocol.RPCMessage) (any, *agentprotocol.RPCError) {
	if message.Method != "session/request_permission" {
		return nil, &agentprotocol.RPCError{Code: -32601, Message: "unsupported server request"}
	}
	var params map[string]any
	_ = json.Unmarshal(message.Params, &params)
	toolCall := objectValue(params["toolCall"])
	id := stringValue(toolCall["toolCallId"])
	s.core.Emit(Event{Kind: EventPermission, Permission: &PermissionRequest{ID: id, Action: stringValue(toolCall["title"]), Detail: toolContent(toolCall["content"])}})
	if s.opts.Permission == "auto" {
		if option := allowedOption(params["options"]); option != "" {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}}, nil
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

func (s *Session) Abort(ctx context.Context) error {
	s.mu.Lock()
	turn := s.active
	if turn == nil {
		s.mu.Unlock()
		return nil
	}
	turn.aborted = true
	done := turn.done
	s.mu.Unlock()
	if err := s.core.RPC().Notify(ctx, "session/cancel", map[string]any{"sessionId": s.NativeSessionID()}); err != nil {
		return s.core.AbortEscalation(fmt.Errorf("session/cancel: %w", err))
	}
	timer := time.NewTimer(s.core.CloseTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return s.core.AbortEscalation(fmt.Errorf("cancel wait: %w", ctx.Err()))
	case <-timer.C:
		return s.core.AbortEscalation(fmt.Errorf("cancel timed out after %s", s.core.CloseTimeout))
	}
}

func (s *Session) Events() <-chan Event { return s.core.Events() }
func (s *Session) NativeSessionID() string {
	return s.core.NativeID()
}
func (s *Session) Alive() bool { return s.core.Alive() }

func (s *Session) Close(ctx context.Context) error { return s.core.Close(ctx) }
