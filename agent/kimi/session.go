// Package kimi drives one persistent native `kimi acp` process.
package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
	agentprotocol "github.com/tianlinzz/agent-cli-gateway/agent/protocol"
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

type Session struct {
	opts    Options
	process *agentprocess.Process
	rpc     *agentprotocol.JSONRPCClient
	events  chan Event
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	sessionID atomic.Value
	alive     atomic.Bool
	closing   atomic.Bool
	close     sync.Once
	wg        sync.WaitGroup
	mu        sync.Mutex
	active    *activePrompt
}

func Start(ctx context.Context, options Options) (*Session, error) {
	if ctx == nil {
		return nil, protocolError("nil context")
	}
	opts := NormalizeOptions(options)
	sessionCtx, cancel := context.WithCancel(ctx)
	command := append([]string(nil), opts.Command...)
	command = append(command, BuildArgs(opts, "", opts.ResumeID)...)
	proc, err := agentprocess.Start(sessionCtx, agentprocess.Spec{Command: command, Dir: opts.WorkDir, Env: opts.Env, Stdin: true})
	if err != nil {
		cancel()
		return nil, protocolError("start ACP: %w", err)
	}
	s := &Session{opts: opts, process: proc, events: make(chan Event, 64), ctx: sessionCtx, cancel: cancel, done: make(chan struct{})}
	s.sessionID.Store("")
	s.rpc = agentprotocol.NewJSONRPCClient(proc.Stdin(), proc.Stdout(), maxACPFrame, s.handleReverse, s.handleNotification, s.onNotifyOverflow)
	if err := s.initialize(ctx); err != nil {
		s.cleanupFailedStart()
		return nil, err
	}
	s.alive.Store(true)
	go s.monitor()
	return s, nil
}

func (s *Session) initialize(ctx context.Context) error {
	var initialized struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := s.rpc.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, &initialized); err != nil {
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
	if err := s.rpc.Call(ctx, method, params, &response); err != nil {
		return protocolError("%s: %w", method, err)
	}
	if strings.TrimSpace(response.SessionID) == "" {
		return protocolError("%s returned an empty session id", method)
	}
	s.sessionID.Store(response.SessionID)
	s.emit(Event{Kind: EventNativeSession, NativeSessionID: response.SessionID})
	return nil
}

func (s *Session) Send(_ context.Context, input Input) error {
	if !s.alive.Load() {
		return protocolError("session is closed")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return protocolError("empty prompt")
	}
	turnCtx, cancel := context.WithTimeout(s.ctx, s.opts.Timeout)
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
	err := s.rpc.Call(ctx, "session/prompt", map[string]any{
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
	s.rpc.Sync()
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
		s.emit(Event{Kind: EventError, Err: promptErr})
		return
	}
	reason := normalizeStopReason(stopReason)
	if aborted {
		reason = "cancelled"
	}
	s.emit(Event{Kind: EventFinish, FinishReason: reason, NativeSessionID: s.NativeSessionID()})
	if usage != nil {
		s.emit(Event{Kind: EventUsage, Usage: usage, NativeSessionID: s.NativeSessionID()})
	}
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
				s.emit(Event{Kind: EventText, Text: text})
			}
		}
	case "agent_thought_chunk":
		content := objectValue(update["content"])
		if stringValue(content["type"]) == "text" {
			if text := stringValue(content["text"]); text != "" {
				s.emit(Event{Kind: EventReasoning, Reasoning: &Reasoning{Text: text}})
			}
		}
	case "tool_call":
		s.handleToolStart(update)
	case "tool_call_update":
		s.handleToolUpdate(update)
	case "usage_update":
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
	s.emit(Event{Kind: EventToolUse, Tool: &tool})
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
		s.emit(Event{Kind: EventToolUse, Tool: &copy})
	}
	tool.Result = toolContent(update["content"])
	tool.IsError = status == "failed"
	s.emit(Event{Kind: EventToolResult, Tool: &tool})
}

func (s *Session) handleReverse(_ context.Context, message agentprotocol.RPCMessage) (any, *agentprotocol.RPCError) {
	if message.Method != "session/request_permission" {
		return nil, &agentprotocol.RPCError{Code: -32601, Message: "unsupported server request"}
	}
	var params map[string]any
	_ = json.Unmarshal(message.Params, &params)
	toolCall := objectValue(params["toolCall"])
	id := stringValue(toolCall["toolCallId"])
	s.emit(Event{Kind: EventPermission, Permission: &PermissionRequest{ID: id, Action: stringValue(toolCall["title"]), Detail: toolContent(toolCall["content"])}})
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
	if err := s.rpc.Notify(ctx, "session/cancel", map[string]any{"sessionId": s.NativeSessionID()}); err != nil {
		return s.abortEscalation(fmt.Errorf("session/cancel: %w", err))
	}
	timer := time.NewTimer(s.opts.Timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return s.abortEscalation(fmt.Errorf("cancel wait: %w", ctx.Err()))
	case <-timer.C:
		return s.abortEscalation(fmt.Errorf("cancel timed out after %s", s.opts.Timeout))
	}
}

func (s *Session) abortEscalation(cause error) error {
	s.closing.Store(true)
	s.alive.Store(false)
	s.cancel()
	_ = s.rpc.Close()
	_ = s.process.ForceKill()
	_ = s.process.Wait()
	return protocolError("abort: %w", cause)
}

func (s *Session) monitor() {
	<-s.rpc.Done()
	waitErr := s.process.Wait()
	s.wg.Wait()
	s.alive.Store(false)
	if !s.closing.Load() {
		message := strings.TrimSpace(s.process.StderrString())
		if message == "" {
			if waitErr != nil {
				message = waitErr.Error()
			} else if err := s.rpc.Err(); err != nil && !errors.Is(err, io.EOF) {
				message = err.Error()
			}
		}
		if message != "" {
			s.emit(Event{Kind: EventError, Err: protocolError("ACP exited: %s", message)})
		}
	}
	s.close.Do(func() {
		// Cancel first so any emitter blocked in emit's select returns, then
		// wait for the JSON-RPC notification drain goroutine to stop calling
		// notify before closing s.events. Otherwise a drain mid-emit would
		// send on a closed channel.
		s.cancel()
		if ch := s.rpc.NotifyDone(); ch != nil {
			<-ch
		}
		close(s.events)
		close(s.done)
	})
}

func (s *Session) cleanupFailedStart() {
	s.closing.Store(true)
	s.cancel()
	_ = s.rpc.Close()
	_ = s.process.ForceKill()
	_ = s.process.Wait()
}

func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

// onNotifyOverflow surfaces a notification-truncation marker when the JSON-RPC
// reader dropped notifications because a stalled consumer saturated the queue.
func (s *Session) onNotifyOverflow(dropped int) {
	s.emit(Event{Kind: EventReasoning, Reasoning: &Reasoning{Text: fmt.Sprintf("[%d notifications truncated]", dropped)}})
}

func (s *Session) Events() <-chan Event { return s.events }
func (s *Session) NativeSessionID() string {
	value, _ := s.sessionID.Load().(string)
	return value
}
func (s *Session) Alive() bool { return s.alive.Load() }

func (s *Session) Close(ctx context.Context) error {
	if !s.alive.Swap(false) {
		select {
		case <-s.done:
			return nil
		default:
		}
	}
	s.closing.Store(true)
	s.cancel()
	_ = s.rpc.Close()
	timer := time.NewTimer(s.opts.Timeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		_ = s.process.ForceKill()
		return protocolError("close wait: %w", ctx.Err())
	case <-timer.C:
		_ = s.process.ForceKill()
		select {
		case <-s.done:
			return nil
		case <-time.After(s.opts.Timeout):
			return protocolError("close timed out after %s", 2*s.opts.Timeout)
		}
	}
}
