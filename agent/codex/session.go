// Package codex drives one persistent native `codex app-server` process.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
	agentprotocol "github.com/tianlinzz/agent-cli-gateway/agent/protocol"
)

const maxAppServerFrame = 10 * 1024 * 1024

type activeTurn struct {
	id      string
	done    chan struct{}
	usage   *Usage
	tools   map[string]ToolCall
	results map[string]bool
	aborted bool
	closed  bool
}

// Session owns one Codex app-server process and one native thread.
type Session struct {
	opts    Options
	process *agentprocess.Process
	rpc     *agentprotocol.JSONRPCClient
	events  chan Event
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	threadID atomic.Value
	alive    atomic.Bool
	closing  atomic.Bool
	close    sync.Once

	mu     sync.Mutex
	active *activeTurn
}

// Start launches and initializes a persistent Codex app-server.
func Start(ctx context.Context, options Options) (*Session, error) {
	if ctx == nil {
		return nil, protocolError("nil context")
	}
	opts := NormalizeOptions(options)
	sessionCtx, cancel := context.WithCancel(ctx)
	command := append([]string(nil), opts.Command...)
	command = append(command, BuildArgs(opts, opts.ResumeID)...)
	proc, err := agentprocess.Start(sessionCtx, agentprocess.Spec{
		Command: command, Dir: opts.WorkDir, Env: opts.Env, Stdin: true,
	})
	if err != nil {
		cancel()
		return nil, protocolError("start app-server: %w", err)
	}

	s := &Session{opts: opts, process: proc, events: make(chan Event, 64), ctx: sessionCtx, cancel: cancel, done: make(chan struct{})}
	s.threadID.Store("")
	s.rpc = agentprotocol.NewJSONRPCClient(proc.Stdin(), proc.Stdout(), maxAppServerFrame, s.handleReverse, s.handleNotification)
	if err := s.initialize(ctx); err != nil {
		s.cleanupFailedStart()
		return nil, err
	}
	s.alive.Store(true)
	go s.monitor()
	return s, nil
}

func (s *Session) initialize(ctx context.Context) error {
	var initialized map[string]any
	if err := s.rpc.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agent-cli-gateway", "title": "Agent CLI Gateway", "version": "dev"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		return protocolError("initialize app-server: %w", err)
	}
	if err := s.rpc.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return protocolError("notify initialized: %w", err)
	}

	params := s.threadParams()
	method := "thread/start"
	if s.opts.ResumeID != "" {
		method = "thread/resume"
		params["threadId"] = s.opts.ResumeID
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := s.rpc.Call(ctx, method, params, &response); err != nil {
		return protocolError("%s: %w", method, err)
	}
	if strings.TrimSpace(response.Thread.ID) == "" {
		return protocolError("%s returned an empty thread id", method)
	}
	s.threadID.Store(response.Thread.ID)
	s.emit(Event{Kind: EventNativeSession, NativeSessionID: response.Thread.ID})
	return nil
}

func (s *Session) threadParams() map[string]any {
	params := map[string]any{
		"cwd":            s.opts.WorkDir,
		"approvalPolicy": approvalPolicy(s.opts.Permission),
		"sandbox":        "danger-full-access",
	}
	putNonEmpty(params, "model", s.opts.Model)
	putNonEmpty(params, "modelProvider", s.opts.ModelProvider)
	putNonEmpty(params, "baseInstructions", s.opts.SystemPrompt)
	putNonEmpty(params, "developerInstructions", s.opts.AppendSystemPrompt)
	if s.opts.BaseURL != "" {
		params["config"] = map[string]any{"openai_base_url": s.opts.BaseURL}
	}
	return params
}

func approvalPolicy(permission string) string {
	if permission == "auto" {
		return "never"
	}
	return "on-request"
}

func putNonEmpty(values map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		values[key] = value
	}
}

// Send starts one turn on the existing native thread.
func (s *Session) Send(ctx context.Context, input Input) error {
	if !s.alive.Load() {
		return protocolError("session is closed")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return protocolError("empty prompt")
	}
	turn := &activeTurn{done: make(chan struct{}), tools: make(map[string]ToolCall), results: make(map[string]bool)}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		return protocolError("turn already active")
	}
	s.active = turn
	s.mu.Unlock()

	params := map[string]any{
		"threadId": s.NativeSessionID(),
		"input":    []map[string]any{{"type": "text", "text": prompt}},
	}
	putNonEmpty(params, "effort", s.opts.ReasoningEffort)
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := s.rpc.Call(ctx, "turn/start", params, &response); err != nil {
		s.clearActive(turn)
		return protocolError("turn/start: %w", err)
	}
	if response.Turn.ID == "" {
		s.clearActive(turn)
		return protocolError("turn/start returned an empty turn id")
	}
	s.mu.Lock()
	if s.active == turn && turn.id == "" {
		turn.id = response.Turn.ID
	}
	s.mu.Unlock()
	return nil
}

func (s *Session) handleNotification(message agentprotocol.RPCMessage) {
	var params map[string]any
	if json.Unmarshal(message.Params, &params) != nil {
		slog.Warn("codex: ignore malformed notification", "method", message.Method)
		return
	}
	switch message.Method {
	case "thread/started":
		if thread, _ := params["thread"].(map[string]any); thread != nil {
			s.updateThreadID(stringValue(thread["id"]))
		}
	case "turn/started":
		turn, _ := params["turn"].(map[string]any)
		s.setActiveTurnID(stringValue(turn["id"]))
	case "item/agentMessage/delta":
		if s.notificationForActive(params) {
			if delta := stringValue(params["delta"]); delta != "" {
				s.emit(Event{Kind: EventText, Text: delta})
			}
		}
	case "item/started":
		if s.notificationForActive(params) {
			s.handleItemStarted(objectValue(params["item"]))
		}
	case "item/completed":
		if s.notificationForActive(params) {
			s.handleItemCompleted(objectValue(params["item"]))
		}
	case "thread/tokenUsage/updated":
		s.recordUsage(params)
	case "turn/completed":
		s.completeTurn(params)
	}
}

func (s *Session) handleReverse(_ context.Context, message agentprotocol.RPCMessage) (any, *agentprotocol.RPCError) {
	var params map[string]any
	_ = json.Unmarshal(message.Params, &params)
	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		id := stringValue(params["itemId"])
		s.emit(Event{Kind: EventPermission, Permission: &PermissionRequest{ID: id, Action: message.Method, Detail: approvalDetail(params)}})
		decision := "decline"
		if s.opts.Permission == "auto" {
			decision = "acceptForSession"
		}
		return map[string]any{"decision": decision}, nil
	case "item/tool/requestUserInput":
		s.emit(Event{Kind: EventError, Err: protocolError("interactive user input is disabled in unattended gateway mode")})
		return map[string]any{"answers": map[string]any{}}, nil
	default:
		return nil, &agentprotocol.RPCError{Code: -32601, Message: "unsupported server request"}
	}
}

func approvalDetail(params map[string]any) string {
	for _, key := range []string{"command", "reason", "grantRoot"} {
		if value := stringValue(params[key]); value != "" {
			return value
		}
	}
	return "Codex requested approval"
}

func (s *Session) handleItemStarted(item map[string]any) {
	tool, ok := toolFromItem(item)
	if !ok {
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

func (s *Session) handleItemCompleted(item map[string]any) {
	kind := stringValue(item["type"])
	if kind == "reasoning" || kind == "agentMessage" {
		return
	}
	tool, ok := toolFromItem(item)
	if !ok {
		return
	}
	s.mu.Lock()
	turn := s.active
	started := false
	if turn != nil {
		if turn.results[tool.ID] {
			s.mu.Unlock()
			return
		}
		turn.results[tool.ID] = true
		_, started = turn.tools[tool.ID]
		if !started {
			turn.tools[tool.ID] = tool
		}
	}
	s.mu.Unlock()
	if !started {
		start := tool
		start.Result = ""
		start.IsError = false
		s.emit(Event{Kind: EventToolUse, Tool: &start})
	}
	tool.Result, tool.IsError = toolResult(item)
	s.emit(Event{Kind: EventToolResult, Tool: &tool})
}

func (s *Session) recordUsage(params map[string]any) {
	if !s.notificationForActive(params) {
		return
	}
	tokenUsage := objectValue(params["tokenUsage"])
	usage := usageFromCamelMap(objectValue(tokenUsage["last"]))
	if usage == nil {
		return
	}
	s.mu.Lock()
	if s.active != nil {
		s.active.usage = usage
	}
	s.mu.Unlock()
}

func (s *Session) completeTurn(params map[string]any) {
	turnValue := objectValue(params["turn"])
	id := stringValue(turnValue["id"])
	s.mu.Lock()
	turn := s.active
	if turn == nil || (turn.id != "" && id != turn.id) {
		s.mu.Unlock()
		return
	}
	if turn.closed {
		s.mu.Unlock()
		return
	}
	turn.closed = true
	usage := turn.usage
	if usage == nil {
		usage = usageFromCamelMap(objectValue(params["usage"]))
	}
	aborted := turn.aborted
	status := stringValue(turnValue["status"])
	deleteActive := s.active == turn
	if deleteActive {
		s.active = nil
	}
	close(turn.done)
	s.mu.Unlock()

	if status == "failed" {
		s.emit(Event{Kind: EventError, Err: protocolError("turn failed: %s", turnError(turnValue))})
		return
	}
	reason := "end_turn"
	if aborted || status == "interrupted" {
		reason = "cancelled"
	}
	s.emit(Event{Kind: EventFinish, FinishReason: reason, NativeSessionID: s.NativeSessionID()})
	if usage != nil {
		s.emit(Event{Kind: EventUsage, Usage: usage, NativeSessionID: s.NativeSessionID()})
	}
}

func turnError(turn map[string]any) string {
	errorValue := objectValue(turn["error"])
	if message := stringValue(errorValue["message"]); message != "" {
		return message
	}
	return "no details"
}

func (s *Session) notificationForActive(params map[string]any) bool {
	id := stringValue(params["turnId"])
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil && (s.active.id == "" || id == "" || s.active.id == id)
}

func (s *Session) setActiveTurnID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	if s.active != nil && s.active.id == "" {
		s.active.id = id
	}
	s.mu.Unlock()
}

func (s *Session) clearActive(turn *activeTurn) {
	s.mu.Lock()
	if s.active == turn {
		s.active = nil
	}
	if !turn.closed {
		turn.closed = true
		close(turn.done)
	}
	s.mu.Unlock()
}

func (s *Session) updateThreadID(id string) {
	if id != "" && id != s.NativeSessionID() {
		s.threadID.Store(id)
		s.emit(Event{Kind: EventNativeSession, NativeSessionID: id})
	}
}

func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *Session) monitor() {
	<-s.rpc.Done()
	waitErr := s.process.Wait()
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
			s.emit(Event{Kind: EventError, Err: protocolError("app-server exited: %s", message)})
		}
	}
	s.close.Do(func() {
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

func (s *Session) Events() <-chan Event { return s.events }

func (s *Session) NativeSessionID() string {
	id, _ := s.threadID.Load().(string)
	return id
}

func (s *Session) Alive() bool { return s.alive.Load() }

// Abort interrupts only the active turn and preserves the app-server process.
func (s *Session) Abort(ctx context.Context) error {
	s.mu.Lock()
	turn := s.active
	if turn == nil {
		s.mu.Unlock()
		return nil
	}
	turn.aborted = true
	id := turn.id
	done := turn.done
	s.mu.Unlock()
	if id == "" {
		return protocolError("abort: active turn has no native id")
	}
	if err := s.rpc.Call(ctx, "turn/interrupt", map[string]any{"threadId": s.NativeSessionID(), "turnId": id}, nil); err != nil {
		return s.abortEscalation(ctx, fmt.Errorf("turn/interrupt: %w", err))
	}
	timer := time.NewTimer(s.opts.CloseTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return s.abortEscalation(ctx, fmt.Errorf("interrupt wait: %w", ctx.Err()))
	case <-timer.C:
		return s.abortEscalation(ctx, fmt.Errorf("interrupt timed out after %s", s.opts.CloseTimeout))
	}
}

func (s *Session) abortEscalation(_ context.Context, cause error) error {
	s.closing.Store(true)
	s.alive.Store(false)
	s.cancel()
	_ = s.rpc.Close()
	_ = s.process.ForceKill()
	_ = s.process.Wait()
	return protocolError("abort: %w", cause)
}

// Close terminates and reaps the persistent app-server once.
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
	timer := time.NewTimer(s.opts.CloseTimeout)
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
		case <-time.After(s.opts.CloseTimeout):
			return protocolError("close timed out after %s", 2*s.opts.CloseTimeout)
		}
	}
}

func objectValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
