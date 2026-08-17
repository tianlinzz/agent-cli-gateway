// Package codex drives one persistent native `codex app-server` process.
package codex

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

const maxAppServerFrame = 10 * 1024 * 1024

type activeTurn struct {
	id   string
	done chan struct{}
	// idPublished is closed once when the native turn id is first set,
	// unblocking an Abort caller that raced turn-start (O-B4).
	idPublished chan struct{}
	usage       *Usage
	tools       map[string]ToolCall
	results     map[string]bool
	aborted     bool
	closed      bool
}

// publishID records the native turn id and signals any Abort caller waiting
// for it. Must be called with the parent Session's mutex held.
func (t *activeTurn) publishID(id string) {
	if id == "" || t.id != "" {
		return
	}
	t.id = id
	if t.idPublished != nil {
		close(t.idPublished)
		t.idPublished = nil // prevent double-close; id != "" guards re-entry
	}
}

// Session owns one Codex app-server process and one native thread. The
// mechanical lifecycle (process, monitor/teardown, event channel, abort
// escalation, Close) lives in the shared rpcsession.Core (D2); this file
// keeps only the Codex protocol specifics.
type Session struct {
	core *rpcsession.Core
	opts Options

	mu     sync.Mutex
	active *activeTurn
}

// Start launches and initializes a persistent Codex app-server.
func Start(ctx context.Context, options Options) (*Session, error) {
	opts := NormalizeOptions(options)
	s := &Session{core: rpcsession.New("codex", "app-server", opts.CloseTimeout, protocolError), opts: opts}
	command := append([]string(nil), opts.Command...)
	command = append(command, BuildArgs(opts, opts.ResumeID)...)
	if err := s.core.Launch(ctx, agentprocess.Spec{Command: command, Dir: opts.WorkDir, Env: opts.Env, Stdin: true}, maxAppServerFrame, s.handleReverse, s.handleNotification, isCriticalCodexNotification); err != nil {
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
	var initialized map[string]any
	if err := s.core.RPC().Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agent-cli-gateway", "title": "Agent CLI Gateway", "version": "dev"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		return protocolError("initialize app-server: %w", err)
	}
	if err := s.core.RPC().Notify(ctx, "initialized", map[string]any{}); err != nil {
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
	if err := s.core.RPC().Call(ctx, method, params, &response); err != nil {
		return protocolError("%s: %w", method, err)
	}
	if strings.TrimSpace(response.Thread.ID) == "" {
		return protocolError("%s returned an empty thread id", method)
	}
	s.core.SetNativeID(response.Thread.ID)
	s.core.Emit(Event{Kind: EventNativeSession, NativeSessionID: response.Thread.ID})
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
	if !s.core.Alive() {
		return protocolError("session is closed")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return protocolError("empty prompt")
	}
	turn := &activeTurn{done: make(chan struct{}), idPublished: make(chan struct{}), tools: make(map[string]ToolCall), results: make(map[string]bool)}
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
	if err := s.core.RPC().Call(ctx, "turn/start", params, &response); err != nil {
		s.clearActive(turn)
		return protocolError("turn/start: %w", err)
	}
	if response.Turn.ID == "" {
		s.clearActive(turn)
		return protocolError("turn/start returned an empty turn id")
	}
	s.mu.Lock()
	if s.active == turn {
		turn.publishID(response.Turn.ID)
	}
	s.mu.Unlock()
	return nil
}

// isCriticalCodexNotification reports whether a notification drives the
// Gateway state machine (turn lifecycle, thread/session identity, usage) and
// must never be dropped to queue overflow. Display notifications (text deltas,
// tool progress) may be truncated under sustained backpressure.
func isCriticalCodexNotification(message agentprotocol.RPCMessage) bool {
	switch message.Method {
	case "turn/started", "turn/completed",
		"thread/started", "thread/tokenUsage/updated":
		return true
	}
	return false
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
				s.core.Emit(Event{Kind: EventText, Text: delta})
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
		s.core.Emit(Event{Kind: EventPermission, Permission: &PermissionRequest{ID: id, Action: message.Method, Detail: approvalDetail(params)}})
		decision := "decline"
		if s.opts.Permission == "auto" {
			decision = "acceptForSession"
		}
		return map[string]any{"decision": decision}, nil
	case "item/tool/requestUserInput":
		s.core.Emit(Event{Kind: EventError, Err: protocolError("interactive user input is disabled in unattended gateway mode")})
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
	if stringValue(item["type"]) == "reasoning" {
		if text := reasoningSummary(item["summary"]); text != "" {
			s.core.Emit(Event{Kind: EventReasoning, Reasoning: &Reasoning{ID: stringValue(item["id"]), Text: text}})
		}
		return
	}
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
	s.core.Emit(Event{Kind: EventToolUse, Tool: &tool})
}

func reasoningSummary(value any) string {
	values, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return strings.Join(parts, "\n")
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
		s.core.Emit(Event{Kind: EventToolUse, Tool: &start})
	}
	tool.Result, tool.IsError = toolResult(item)
	s.core.Emit(Event{Kind: EventToolResult, Tool: &tool})
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
	// turn/completed arrives on the wire after the turn's item/* notifications.
	// It is routed through the critical (never-dropped) path, so it may be
	// processed on a separate goroutine from the display drain. Wait for the
	// display queue to flush the turn's item events before finalizing, so
	// finish never precedes tool/text events the consumer reads up to finish.
	s.core.RPC().Sync()
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

	// Converged abort semantics (D2): an aborted turn ALWAYS ends with
	// finish("cancelled"). A native failure on an aborted turn is the
	// interrupt's side effect, not a reportable run error — log it, never
	// emit an error event (which the API would record as RunFailed).
	if status == "failed" {
		if aborted {
			slog.Warn("codex: turn failed after abort", "thread_id", s.NativeSessionID(), "detail", turnError(turnValue))
			s.core.Emit(Event{Kind: EventFinish, FinishReason: "cancelled", NativeSessionID: s.NativeSessionID()})
			if usage != nil {
				s.core.Emit(Event{Kind: EventUsage, Usage: usage, NativeSessionID: s.NativeSessionID()})
			}
			return
		}
		s.core.Emit(Event{Kind: EventError, Err: protocolError("turn failed: %s", turnError(turnValue))})
		return
	}
	reason := "end_turn"
	if aborted || status == "interrupted" {
		reason = "cancelled"
	}
	s.core.Emit(Event{Kind: EventFinish, FinishReason: reason, NativeSessionID: s.NativeSessionID()})
	if usage != nil {
		s.core.Emit(Event{Kind: EventUsage, Usage: usage, NativeSessionID: s.NativeSessionID()})
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
	if s.active != nil {
		s.active.publishID(id)
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
		s.core.SetNativeID(id)
		s.core.Emit(Event{Kind: EventNativeSession, NativeSessionID: id})
	}
}

func (s *Session) Events() <-chan Event { return s.core.Events() }

func (s *Session) NativeSessionID() string { return s.core.NativeID() }

func (s *Session) Alive() bool { return s.core.Alive() }

// Abort interrupts only the active turn and preserves the app-server process.
// If the turn id has not yet been published (abort racing turn-start), Abort
// waits for publication instead of hard-failing, then proceeds with the
// protocol-level interrupt or escalates after a bounded timeout (O-B4).
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
	idPublished := turn.idPublished
	s.mu.Unlock()
	if id == "" {
		// The turn id hasn't been published yet (abort concurrent with
		// turn/start). Wait for publication rather than returning a hard
		// error that would cause the caller to tear down the entire
		// persistent app-server.
		timer := time.NewTimer(s.core.CloseTimeout)
		defer timer.Stop()
		select {
		case <-idPublished:
			s.mu.Lock()
			id = turn.id
			s.mu.Unlock()
		case <-done:
			return nil // turn completed before id was published
		case <-ctx.Done():
			return s.core.AbortEscalation(fmt.Errorf("abort wait: %w", ctx.Err()))
		case <-timer.C:
			return s.core.AbortEscalation(fmt.Errorf("abort: turn id not published within %s", s.core.CloseTimeout))
		}
		if id == "" {
			return nil // publication signalled but id still empty — nothing to cancel
		}
	}
	if err := s.core.RPC().Call(ctx, "turn/interrupt", map[string]any{"threadId": s.NativeSessionID(), "turnId": id}, nil); err != nil {
		return s.core.AbortEscalation(fmt.Errorf("turn/interrupt: %w", err))
	}
	timer := time.NewTimer(s.core.CloseTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return s.core.AbortEscalation(fmt.Errorf("interrupt wait: %w", ctx.Err()))
	case <-timer.C:
		return s.core.AbortEscalation(fmt.Errorf("interrupt timed out after %s", s.core.CloseTimeout))
	}
}

// Close terminates and reaps the persistent app-server once.
func (s *Session) Close(ctx context.Context) error { return s.core.Close(ctx) }

func objectValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
