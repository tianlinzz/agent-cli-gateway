// Package codex drives the native `codex exec --json` protocol.
package codex

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
	agentprotocol "github.com/tianlinzz/agent-cli-gateway/agent/protocol"
)

// Session owns a resumable conversation and one process per active turn.
type Session struct {
	opts     Options
	ctx      context.Context
	cancel   context.CancelFunc
	events   chan Event
	threadID atomic.Value
	alive    atomic.Bool

	mu       sync.Mutex
	inFlight *agentprocess.Process
	turnDone map[*agentprocess.Process]chan struct{}
	aborted  map[*agentprocess.Process]bool
	wg       sync.WaitGroup
	close    sync.Once
}

// New constructs an idle resume-per-turn native session.
func New(options Options) *Session {
	opts := NormalizeOptions(options)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		opts: opts, ctx: ctx, cancel: cancel, events: make(chan Event, 64),
		turnDone: make(map[*agentprocess.Process]chan struct{}),
		aborted:  make(map[*agentprocess.Process]bool),
	}
	s.threadID.Store(opts.ResumeID)
	s.alive.Store(true)
	return s
}

// Send launches one Codex process for the turn.
func (s *Session) Send(ctx context.Context, input Input) error {
	if !s.alive.Load() {
		return protocolError("session is closed")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return protocolError("empty prompt")
	}
	threadID := s.NativeSessionID()
	if threadID == "" {
		prompt = prependPreamble(prompt, promptPreamble(s.opts.SystemPrompt, s.opts.AppendSystemPrompt))
	}

	s.mu.Lock()
	previous := s.inFlight
	previousDone := s.turnDone[previous]
	s.mu.Unlock()
	if previousDone != nil {
		select {
		case <-previousDone:
		case <-ctx.Done():
			return protocolError("wait previous turn: %w", ctx.Err())
		}
	}
	s.mu.Lock()
	if s.inFlight != nil {
		s.mu.Unlock()
		return protocolError("turn already active")
	}
	s.mu.Unlock()

	command := append([]string(nil), s.opts.Command...)
	command = append(command, BuildArgs(s.opts, threadID)...)
	proc, err := agentprocess.Start(s.ctx, agentprocess.Spec{
		Command: command, Dir: s.opts.WorkDir, Env: s.opts.Env, Stdin: true,
	})
	if err != nil {
		return protocolError("start turn: %w", err)
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.inFlight = proc
	s.turnDone[proc] = done
	s.mu.Unlock()

	if _, err := io.WriteString(proc.Stdin(), prompt); err != nil {
		_ = proc.ForceKill()
		_ = proc.Wait()
		s.clearTurn(proc)
		return protocolError("write prompt: %w", err)
	}
	if err := proc.CloseStdin(); err != nil {
		_ = proc.ForceKill()
		_ = proc.Wait()
		s.clearTurn(proc)
		return protocolError("close prompt stdin: %w", err)
	}

	s.wg.Add(1)
	go s.readTurn(proc)
	return nil
}

type turnState struct {
	pending []string
	usage   *Usage
}

func (s *Session) readTurn(proc *agentprocess.Process) {
	defer s.wg.Done()
	defer s.clearTurn(proc)
	state := &turnState{}
	decoder := agentprotocol.NewJSONLDecoder(proc.Stdout(), 10*1024*1024)
	for {
		var raw map[string]any
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !s.wasAborted(proc) {
				s.emit(Event{Kind: EventError, Err: protocolError("decode stream: %w", err)})
			}
			_ = proc.ForceKill()
			break
		}
		s.handleEvent(state, raw)
	}
	if err := proc.Wait(); err != nil && !s.wasAborted(proc) && s.alive.Load() {
		message := strings.TrimSpace(proc.StderrString())
		if message == "" {
			message = err.Error()
		}
		s.emit(Event{Kind: EventError, Err: protocolError("process exited: %s", message)})
	}
}

func (s *Session) handleEvent(state *turnState, raw map[string]any) {
	eventType, _ := raw["type"].(string)
	switch eventType {
	case "thread.started":
		if id, _ := raw["thread_id"].(string); id != "" && id != s.NativeSessionID() {
			s.threadID.Store(id)
			s.emit(Event{Kind: EventNativeSession, NativeSessionID: id})
		}
	case "turn.started":
		state.pending = nil
		state.usage = nil
	case "item.started":
		s.handleItemStarted(state, raw)
	case "item.completed":
		s.handleItemCompleted(state, raw)
	case "turn.completed":
		s.flushPending(state)
		state.usage = usageFromMap(raw["usage"])
		if state.usage == nil {
			if path := findRollout(s.opts.Env, s.NativeSessionID()); path != "" {
				state.usage, _ = ReadRolloutUsage(path)
			}
		}
		s.emit(Event{Kind: EventFinish, FinishReason: "end_turn", NativeSessionID: s.NativeSessionID()})
		if state.usage != nil {
			s.emit(Event{Kind: EventUsage, Usage: state.usage, NativeSessionID: s.NativeSessionID()})
		}
	case "turn.failed":
		message := "turn failed (no details)"
		if object, ok := raw["error"].(map[string]any); ok {
			if value, _ := object["message"].(string); value != "" {
				message = value
			}
		}
		s.emit(Event{Kind: EventError, Err: protocolError("%s", message)})
	case "approval.requested":
		id, _ := raw["id"].(string)
		if id == "" {
			id, _ = raw["request_id"].(string)
		}
		action, _ := raw["action"].(string)
		detail, _ := raw["detail"].(string)
		s.emit(Event{Kind: EventPermission, Permission: &PermissionRequest{ID: id, Action: action, Detail: detail}})
	}
}

func (s *Session) handleItemStarted(state *turnState, raw map[string]any) {
	item, _ := raw["item"].(map[string]any)
	itemType, _ := item["type"].(string)
	if itemType == "agent_message" || itemType == "message" || itemType == "reasoning" {
		return
	}
	s.flushPending(state)
	switch itemType {
	case "command_execution":
		command, _ := item["command"].(string)
		s.emit(Event{Kind: EventToolUse, Tool: &ToolCall{Name: "Bash", Arguments: toolArguments(itemType, command)}})
	case "function_call":
		name, _ := item["name"].(string)
		arguments, _ := item["arguments"].(string)
		s.emit(Event{Kind: EventToolUse, Tool: &ToolCall{Name: name, Arguments: toolArguments(itemType, arguments)}})
	}
}

func (s *Session) handleItemCompleted(state *turnState, raw map[string]any) {
	item, _ := raw["item"].(map[string]any)
	itemType, _ := item["type"].(string)
	switch itemType {
	case "reasoning":
		// Reasoning summaries are native diagnostics, not assistant output.
		return
	case "agent_message", "message":
		if text := extractItemText(item, "content", "output_text"); text != "" {
			state.pending = append(state.pending, text)
		}
	case "command_execution":
		status, _ := item["status"].(string)
		output, _ := item["aggregated_output"].(string)
		exitCode := intValue(item["exit_code"])
		s.emit(Event{Kind: EventToolResult, Tool: &ToolCall{Name: "Bash", Result: truncate(strings.TrimSpace(output), 500), IsError: !toolSuccess(status, &exitCode)}})
	case "function_call":
		name, _ := item["name"].(string)
		status, _ := item["status"].(string)
		output, _ := item["output"].(string)
		s.emit(Event{Kind: EventToolResult, Tool: &ToolCall{Name: name, Result: truncate(strings.TrimSpace(output), 500), IsError: !toolSuccess(status, nil)}})
	default:
		if name, ok := toolNames[itemType]; ok {
			s.emit(Event{Kind: EventToolUse, Tool: &ToolCall{Name: name, Arguments: map[string]any{"input": toolInput(item)}}})
		}
	}
}

func (s *Session) flushPending(state *turnState) {
	for _, text := range state.pending {
		s.emit(Event{Kind: EventText, Text: text})
	}
	state.pending = nil
}

func (s *Session) clearTurn(proc *agentprocess.Process) {
	s.mu.Lock()
	done := s.turnDone[proc]
	delete(s.turnDone, proc)
	delete(s.aborted, proc)
	if s.inFlight == proc {
		s.inFlight = nil
	}
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (s *Session) wasAborted(proc *agentprocess.Process) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted[proc]
}

func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *Session) Events() <-chan Event { return s.events }
func (s *Session) NativeSessionID() string {
	value, _ := s.threadID.Load().(string)
	return value
}
func (s *Session) Alive() bool { return s.alive.Load() }

// Abort kills and reaps only the active turn process.
func (s *Session) Abort(ctx context.Context) error {
	s.mu.Lock()
	proc := s.inFlight
	if proc == nil {
		s.mu.Unlock()
		return nil
	}
	s.aborted[proc] = true
	done := s.turnDone[proc]
	s.mu.Unlock()
	if err := proc.ForceKill(); err != nil {
		return protocolError("abort: %w", err)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return protocolError("abort wait: %w", ctx.Err())
	}
}

// Close terminates any active turn and closes the event stream once.
func (s *Session) Close(ctx context.Context) error {
	if !s.alive.Swap(false) {
		return nil
	}
	s.cancel()
	s.mu.Lock()
	proc := s.inFlight
	s.mu.Unlock()
	if proc != nil {
		_ = proc.ForceKill()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.opts.CloseTimeout):
		return protocolError("close timed out after %s", s.opts.CloseTimeout)
	case <-ctx.Done():
		return protocolError("close wait: %w", ctx.Err())
	}
	s.close.Do(func() { close(s.events) })
	return nil
}
