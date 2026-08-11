// Package kimi drives native Kimi Code CLI sessions.
package kimi

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

type Session struct {
	opts       Options
	ctx        context.Context
	cancel     context.CancelFunc
	events     chan Event
	sessionID  atomic.Value
	alive      atomic.Bool
	mu         sync.Mutex
	inFlight   *agentprocess.Process
	turnDone   map[*agentprocess.Process]chan struct{}
	turnCancel map[*agentprocess.Process]context.CancelFunc
	aborted    map[*agentprocess.Process]bool
	wg         sync.WaitGroup
	close      sync.Once
}

func New(options Options) *Session {
	opts := NormalizeOptions(options)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{opts: opts, ctx: ctx, cancel: cancel, events: make(chan Event, 64), turnDone: map[*agentprocess.Process]chan struct{}{}, turnCancel: map[*agentprocess.Process]context.CancelFunc{}, aborted: map[*agentprocess.Process]bool{}}
	s.sessionID.Store(opts.ResumeID)
	s.alive.Store(true)
	return s
}

func (s *Session) Send(ctx context.Context, input Input) error {
	if !s.alive.Load() {
		return protocolError("session is closed")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return protocolError("empty prompt")
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

	turnCtx, cancel := context.WithCancel(s.ctx)
	if s.opts.Timeout > 0 {
		turnCtx, cancel = context.WithTimeout(s.ctx, s.opts.Timeout)
	}
	command := append([]string(nil), s.opts.Command...)
	command = append(command, BuildArgs(s.opts, prompt, s.NativeSessionID())...)
	proc, err := agentprocess.Start(turnCtx, agentprocess.Spec{Command: command, Dir: s.opts.WorkDir, Env: s.opts.Env})
	if err != nil {
		cancel()
		return protocolError("start turn: %w", err)
	}
	done := make(chan struct{})
	s.mu.Lock()
	if s.inFlight != nil {
		s.mu.Unlock()
		cancel()
		_ = proc.ForceKill()
		_ = proc.Wait()
		return protocolError("turn already active")
	}
	s.inFlight = proc
	s.turnDone[proc] = done
	s.turnCancel[proc] = cancel
	s.mu.Unlock()
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
	waitErr := proc.Wait()
	stderr := proc.StderrString()
	if id := extractResumeID(stderr); id != "" && id != s.NativeSessionID() {
		s.sessionID.Store(id)
		s.emit(Event{Kind: EventNativeSession, NativeSessionID: id})
	}
	if waitErr != nil && !s.wasAborted(proc) && s.alive.Load() {
		s.emit(Event{Kind: EventError, Err: protocolError("process exited: %s", strings.TrimSpace(stderr))})
		return
	}
	s.flush(state)
	s.emit(Event{Kind: EventFinish, FinishReason: "end_turn", NativeSessionID: s.NativeSessionID()})
	if state.usage != nil {
		s.emit(Event{Kind: EventUsage, Usage: state.usage, NativeSessionID: s.NativeSessionID()})
	}
}

func (s *Session) handleEvent(state *turnState, raw map[string]any) {
	role, _ := raw["role"].(string)
	switch role {
	case "assistant":
		if usage := usageFromValue(raw["usage"]); usage != nil {
			state.usage = usage
		}
		content, _ := raw["content"].([]any)
		for _, value := range content {
			block, ok := value.(map[string]any)
			if !ok {
				continue
			}
			switch kind, _ := block["type"].(string); kind {
			case "think", "thinking":
				// Keep private reasoning out of OpenAI assistant content.
				continue
			case "text":
				if text, _ := block["text"].(string); text != "" {
					state.pending = append(state.pending, text)
				}
			}
		}
		calls, _ := raw["tool_calls"].([]any)
		if len(calls) > 0 {
			s.flush(state)
		}
		for _, value := range calls {
			call, _ := value.(map[string]any)
			function, _ := call["function"].(map[string]any)
			id, _ := call["id"].(string)
			name, _ := function["name"].(string)
			args, _ := function["arguments"].(string)
			s.emit(Event{Kind: EventToolUse, Tool: &ToolCall{ID: id, Name: name, Arguments: toolArgs(args)}})
		}
	case "tool":
		id, _ := raw["tool_call_id"].(string)
		content, _ := raw["content"].([]any)
		var parts []string
		for _, value := range content {
			if block, ok := value.(map[string]any); ok {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		s.emit(Event{Kind: EventToolResult, Tool: &ToolCall{ID: id, Result: strings.Join(parts, "\n")}})
	}
}

func (s *Session) flush(state *turnState) {
	for _, text := range state.pending {
		s.emit(Event{Kind: EventText, Text: text})
	}
	state.pending = nil
}
func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}
func (s *Session) Events() <-chan Event    { return s.events }
func (s *Session) NativeSessionID() string { value, _ := s.sessionID.Load().(string); return value }
func (s *Session) Alive() bool             { return s.alive.Load() }

func (s *Session) clearTurn(proc *agentprocess.Process) {
	s.mu.Lock()
	done := s.turnDone[proc]
	cancel := s.turnCancel[proc]
	delete(s.turnDone, proc)
	delete(s.turnCancel, proc)
	delete(s.aborted, proc)
	if s.inFlight == proc {
		s.inFlight = nil
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		close(done)
	}
}
func (s *Session) wasAborted(proc *agentprocess.Process) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted[proc]
}

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
	case <-ctx.Done():
		return protocolError("close wait: %w", ctx.Err())
	case <-time.After(8 * time.Second):
		return protocolError("close timed out")
	}
	s.close.Do(func() { close(s.events) })
	return nil
}
