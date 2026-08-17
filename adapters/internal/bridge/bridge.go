// Package bridge holds the behavior that is contractually identical across
// every adapter (D3): the runtime session wrapper around a native session
// (event mapping, prompt assembly, resume-ID lookup, turn serialization) and
// the worker-boundary turn-deadline enforcement (O-F09b).
//
// Native protocol logic — launch, framing, abort mechanics, usage decoding —
// stays in agent/<name>; an adapter package contributes only its Options,
// descriptor, and native-options construction.
//
// The wrapper enforces the one-active-turn-per-session rule with a mutex
// (claudecode's pre-bridge wrapper omitted it) and maps the shared native
// event contract (agent/events) onto runtime.Event, the only shape that
// crosses the API boundary.
//
// Turn deadline (O-F09b): when WrapOptions.TurnTimeout is positive, every
// Send starts a deadline watcher. On expiry the watcher aborts the native
// turn; if the native terminal settles within the settle bound the wrapper
// synthesizes finish(timeout) in place of the native terminal, otherwise it
// emits the runtime.TurnDeadlineExceeded error sentinel (outcome unknown).
// This is the single enforcement point shared by ALL agents — native abort
// differences stay inside each agent/<name> Session.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/agent/events"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// Default bounds for the deadline watcher. The native sessions bound their
// own settle/escalation internally (CloseTimeout), so abortBound is only a
// backstop. Each Session snapshots them at Wrap time (tests may shorten the
// per-session copies).
const (
	defaultAbortBound  = 30 * time.Second
	defaultSettleBound = 30 * time.Second
)

// NativeSession is the adapter-facing view of a native agent session. The
// concrete type is the agent/<name> session; adapters and tests may substitute
// fakes.
type NativeSession interface {
	Send(context.Context, events.Input) error
	Events() <-chan events.Event
	Abort(context.Context) error
	Close(context.Context) error
}

// WrapOptions configures the runtime session wrapper.
type WrapOptions struct {
	// Adapter names the adapter for synthesized error messages
	// ("<adapter>: unknown native event ...").
	Adapter string
	// InjectSystemPrompt forwards caller-supplied system role messages into
	// the native prompt at each turn. Default false: system messages are
	// ignored so a client cannot pollute the agent's own tool/skill surface.
	InjectSystemPrompt bool
	// TurnTimeout is the per-turn deadline enforced at this worker boundary
	// (O-F09b). Zero disables the watcher (no bound).
	TurnTimeout time.Duration
}

// Session adapts one native session to the runtime contract. It owns a
// buffered runtime-event channel that closes when the native event stream
// ends, and serializes Send so one turn is delivered at a time.
type Session struct {
	native       NativeSession
	events       chan runtime.Event
	adapter      string
	injectSystem bool
	turnTimeout  time.Duration

	// mu serializes Send: the runtime contract is one active turn per
	// session.
	mu sync.Mutex

	// turnMu guards the active turn watch. t.done is closed exactly once, by
	// whoever clears s.turn under turnMu.
	turnMu sync.Mutex
	turn   *turnWatch

	// emitMu serializes event emission against channel close so the deadline
	// watcher can never send on a closed channel.
	emitMu       sync.Mutex
	eventsClosed bool

	// abortBound/settleBound bound the deadline watcher; snapshotted at Wrap
	// so tests can shorten them per session.
	abortBound  time.Duration
	settleBound time.Duration
}

// turnWatch tracks the active turn for deadline enforcement.
type turnWatch struct {
	done  chan struct{}
	timer *time.Timer
	fired bool // deadline fired (guarded by turnMu)
}

// Wrap starts the event forwarder around native and returns the runtime
// session.
func Wrap(native NativeSession, opts WrapOptions) *Session {
	s := &Session{
		native:       native,
		events:       make(chan runtime.Event, 64),
		adapter:      opts.Adapter,
		injectSystem: opts.InjectSystemPrompt,
		turnTimeout:  opts.TurnTimeout,
		abortBound:   defaultAbortBound,
		settleBound:  defaultSettleBound,
	}
	go func() {
		defer s.closeEvents()
		// Runs before closeEvents (LIFO): release the active watch without
		// synthesizing an event when the native stream ends.
		defer s.endTurnSilently()
		for event := range native.Events() {
			if isTerminal(event) {
				s.emitEvent(s.terminalEvent(event))
				continue
			}
			s.emitEvent(MapEvent(event, opts.Adapter))
		}
	}()
	return s
}

func (s *Session) Send(ctx context.Context, input runtime.Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnTimeout > 0 {
		s.beginTurn()
	}
	return s.native.Send(ctx, events.Input{Prompt: PromptForNative(input, s.injectSystem)})
}

func (s *Session) Events() <-chan runtime.Event    { return s.events }
func (s *Session) Abort(ctx context.Context) error { return s.native.Abort(ctx) }
func (s *Session) Close(ctx context.Context) error { return s.native.Close(ctx) }

// beginTurn arms the deadline watch for a new turn. Called with s.mu held.
func (s *Session) beginTurn() {
	s.turnMu.Lock()
	if s.turn != nil {
		// One active turn per session is enforced upstream; a stale watch
		// means a contract violation. Replace it and let its watcher exit.
		slog.Warn("bridge: replacing still-active turn watch (turn overlap is a contract violation)", "adapter", s.adapter)
		s.clearTurnLocked()
	}
	t := &turnWatch{done: make(chan struct{}), timer: time.NewTimer(s.turnTimeout)}
	s.turn = t
	s.turnMu.Unlock()
	go s.watchTurn(t)
}

// clearTurnLocked releases the active watch. Caller holds turnMu.
func (s *Session) clearTurnLocked() {
	t := s.turn
	if t == nil {
		return
	}
	s.turn = nil
	t.timer.Stop()
	close(t.done)
}

// endTurnSilently releases the active watch when the native stream ends
// without synthesizing an event.
func (s *Session) endTurnSilently() {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.clearTurnLocked()
}

// terminalEvent maps the turn's terminal native event. When the deadline
// fired, the native terminal is only the abort's confirmation and is replaced
// by the synthesized finish(timeout).
func (s *Session) terminalEvent(event events.Event) runtime.Event {
	s.turnMu.Lock()
	t := s.turn
	if t == nil {
		s.turnMu.Unlock()
		return MapEvent(event, s.adapter)
	}
	s.turn = nil
	fired := t.fired
	if !fired {
		t.timer.Stop()
	}
	close(t.done)
	s.turnMu.Unlock()
	if fired {
		return runtime.Event{Type: runtime.EventFinish, FinishReason: runtime.FinishReasonTimeout}
	}
	return MapEvent(event, s.adapter)
}

// watchTurn enforces the deadline for one turn: on expiry it aborts the
// native turn (which settles or escalates internally), waits for the
// terminal, and only declares an unsettled deadline via the sentinel error.
func (s *Session) watchTurn(t *turnWatch) {
	defer t.timer.Stop()
	select {
	case <-t.done:
		return
	case <-t.timer.C:
	}
	s.turnMu.Lock()
	if s.turn != t {
		s.turnMu.Unlock()
		return
	}
	t.fired = true
	s.turnMu.Unlock()

	abortCtx, cancel := context.WithTimeout(context.Background(), s.abortBound)
	abortErr := s.native.Abort(abortCtx)
	cancel()

	settle := time.NewTimer(s.settleBound)
	defer settle.Stop()
	select {
	case <-t.done:
		// The forwarder handled the terminal and emitted finish(timeout).
		return
	case <-settle.C:
	}

	s.turnMu.Lock()
	if s.turn != t {
		s.turnMu.Unlock()
		return
	}
	s.turn = nil
	close(t.done)
	s.turnMu.Unlock()

	if abortErr != nil {
		slog.Warn("bridge: turn deadline abort failed", "adapter", s.adapter, "error", abortErr)
	}
	// The turn's true completion outcome is unknown; surface the sentinel so
	// the API records RunOutcomeUnknown and presents the unified timeout
	// error.
	s.emitEvent(runtime.Event{Type: runtime.EventError, Error: runtime.TurnDeadlineExceeded})
}

func isTerminal(event events.Event) bool {
	return event.Kind == events.EventFinish || event.Kind == events.EventError
}

// emitEvent sends one runtime event, never racing the channel close.
func (s *Session) emitEvent(event runtime.Event) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.eventsClosed {
		return
	}
	s.events <- event
}

func (s *Session) closeEvents() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.eventsClosed = true
	close(s.events)
}

// ResumeID extracts the server-owned native session ID from start metadata
// (O-F03): client metadata is sanitized upstream, so the only legitimate
// source is the gateway-injected native_session_id. The per-adapter legacy
// keys (codex_thread_id and friends) are deliberately not consulted.
func ResumeID(metadata map[string]string) string {
	return strings.TrimSpace(metadata["native_session_id"])
}

// PromptForNative builds the native prompt from a canonical turn. When
// injectSystem is true the caller's system role messages are prepended to the
// latest user message; otherwise only the latest user message is forwarded so
// a client cannot pollute the agent's own tool/skill surface.
func PromptForNative(input runtime.Input, injectSystem bool) string {
	var parts []string
	if injectSystem {
		for _, message := range input.Messages {
			if message.Role == "system" && strings.TrimSpace(message.Content) != "" {
				parts = append(parts, message.Content)
			}
		}
	}
	if user := LastUserMessage(input); user != "" {
		parts = append(parts, user)
	}
	return strings.Join(parts, "\n\n")
}

// LastUserMessage returns the latest non-empty user message of the turn.
func LastUserMessage(input runtime.Input) string {
	var prompt string
	for _, message := range input.Messages {
		if message.Role == "user" && strings.TrimSpace(message.Content) != "" {
			prompt = message.Content
		}
	}
	return prompt
}

// MapEvent maps one native event onto the canonical runtime event. adapter
// prefixes synthesized error messages.
func MapEvent(event events.Event, adapter string) runtime.Event {
	mapped := runtime.Event{NativeSessionID: event.NativeSessionID}
	switch event.Kind {
	case events.EventText:
		mapped.Type, mapped.Text = runtime.EventText, event.Text
	case events.EventReasoning:
		mapped.Type = runtime.EventReasoning
		if event.Reasoning != nil {
			mapped.Reasoning = &runtime.Reasoning{ID: event.Reasoning.ID, Text: event.Reasoning.Text}
		}
	case events.EventToolUse:
		mapped.Type, mapped.Tool = runtime.EventToolUse, mapTool(event.Tool)
	case events.EventToolResult:
		mapped.Type, mapped.Tool = runtime.EventToolResult, mapTool(event.Tool)
	case events.EventPermission:
		mapped.Type = runtime.EventPermission
		if event.Permission != nil {
			mapped.Permission = &runtime.PermissionRequest{ID: event.Permission.ID, Action: event.Permission.Action, Detail: event.Permission.Detail}
		}
	case events.EventUsage:
		mapped.Type = runtime.EventUsage
		if event.Usage != nil {
			mapped.Usage = &runtime.Usage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens, TotalTokens: event.Usage.TotalTokens}
		}
	case events.EventError:
		mapped.Type = runtime.EventError
		if event.Err != nil {
			mapped.Error = event.Err.Error()
		} else {
			mapped.Error = adapter + ": native execution failed"
		}
	case events.EventFinish:
		mapped.Type, mapped.FinishReason = runtime.EventFinish, event.FinishReason
	case events.EventNativeSession:
		mapped.Type = runtime.EventNativeSession
	default:
		mapped.Type, mapped.Error = runtime.EventError, fmt.Sprintf("%s: unknown native event %q", adapter, event.Kind)
	}
	return mapped
}

func mapTool(tool *events.ToolCall) *runtime.ToolCall {
	if tool == nil {
		return nil
	}
	return &runtime.ToolCall{ID: tool.ID, Name: tool.Name, Arguments: tool.Arguments, Result: tool.Result, IsError: tool.IsError}
}
