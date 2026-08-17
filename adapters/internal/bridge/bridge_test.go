package bridge

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/agent/events"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// fakeNative is an in-memory native session: it records the prompts it is fed
// and replays a scripted event stream.
type fakeNative struct {
	mu     sync.Mutex
	prompt events.Input
	aborts int
	closes int
	src    chan events.Event
}

func newFakeNative() *fakeNative {
	return &fakeNative{src: make(chan events.Event, 16)}
}

func (f *fakeNative) Send(_ context.Context, input events.Input) error {
	f.mu.Lock()
	f.prompt = input
	f.mu.Unlock()
	return nil
}
func (f *fakeNative) Events() <-chan events.Event { return f.src }
func (f *fakeNative) Abort(context.Context) error {
	f.mu.Lock()
	f.aborts++
	f.mu.Unlock()
	return nil
}
func (f *fakeNative) Close(context.Context) error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	return nil
}

func TestMapEventTable(t *testing.T) {
	tool := &events.ToolCall{ID: "tool-1", Name: "Bash", Arguments: map[string]any{"command": "pwd"}, Result: "/workspace", IsError: true}
	cases := []struct {
		name  string
		event events.Event
		want  runtime.Event
	}{
		{
			name:  "text",
			event: events.Event{Kind: events.EventText, Text: "hello", NativeSessionID: "native-1"},
			want:  runtime.Event{Type: runtime.EventText, Text: "hello", NativeSessionID: "native-1"},
		},
		{
			name:  "reasoning",
			event: events.Event{Kind: events.EventReasoning, Reasoning: &events.Reasoning{ID: "r1", Text: "summary"}},
			want:  runtime.Event{Type: runtime.EventReasoning, Reasoning: &runtime.Reasoning{ID: "r1", Text: "summary"}},
		},
		{
			name:  "reasoning nil payload",
			event: events.Event{Kind: events.EventReasoning},
			want:  runtime.Event{Type: runtime.EventReasoning},
		},
		{
			name:  "tool use",
			event: events.Event{Kind: events.EventToolUse, Tool: tool},
			want:  runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{ID: "tool-1", Name: "Bash", Arguments: tool.Arguments, Result: "/workspace", IsError: true}},
		},
		{
			name:  "tool result nil payload",
			event: events.Event{Kind: events.EventToolResult},
			want:  runtime.Event{Type: runtime.EventToolResult},
		},
		{
			name:  "permission",
			event: events.Event{Kind: events.EventPermission, Permission: &events.PermissionRequest{ID: "p1", Action: "Bash", Detail: "pwd"}},
			want:  runtime.Event{Type: runtime.EventPermission, Permission: &runtime.PermissionRequest{ID: "p1", Action: "Bash", Detail: "pwd"}},
		},
		{
			name:  "usage",
			event: events.Event{Kind: events.EventUsage, Usage: &events.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}},
			want:  runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}},
		},
		{
			name:  "error with cause",
			event: events.Event{Kind: events.EventError, Err: errors.New("boom")},
			want:  runtime.Event{Type: runtime.EventError, Error: "boom"},
		},
		{
			name:  "error without cause is adapter-prefixed",
			event: events.Event{Kind: events.EventError},
			want:  runtime.Event{Type: runtime.EventError, Error: "codex: native execution failed"},
		},
		{
			name:  "finish",
			event: events.Event{Kind: events.EventFinish, FinishReason: "end_turn", NativeSessionID: "native-1"},
			want:  runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn", NativeSessionID: "native-1"},
		},
		{
			name:  "native session",
			event: events.Event{Kind: events.EventNativeSession, NativeSessionID: "native-1"},
			want:  runtime.Event{Type: runtime.EventNativeSession, NativeSessionID: "native-1"},
		},
		{
			name:  "unknown kind maps to adapter error",
			event: events.Event{Kind: events.EventKind("mystery")},
			want:  runtime.Event{Type: runtime.EventError, Error: `kimi: unknown native event "mystery"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := "codex"
			if tc.name == "error without cause is adapter-prefixed" {
				adapter = "codex"
			}
			if tc.name == "unknown kind maps to adapter error" {
				adapter = "kimi"
			}
			if got := MapEvent(tc.event, adapter); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MapEvent(%#v) = %#v, want %#v", tc.event, got, tc.want)
			}
		})
	}
}

func TestPromptForNative(t *testing.T) {
	input := runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "be exact"},
		{Role: "system", Content: "   "},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "tool", Content: "tool result"},
		{Role: "user", Content: "second"},
	}}
	if got := PromptForNative(input, false); got != "second" {
		t.Fatalf("prompt = %q, want only latest user message", got)
	}
	if got := PromptForNative(input, true); got != "be exact\n\nsecond" {
		t.Fatalf("prompt = %q, want system prepended", got)
	}
	if got := PromptForNative(runtime.Input{}, true); got != "" {
		t.Fatalf("prompt = %q, want empty", got)
	}
}

func TestResumeIDIsServerOwned(t *testing.T) {
	metadata := map[string]string{
		"native_session_id": "  server-owned-1  ",
		"codex_thread_id":   "client-injected",
		"kimi_session_id":   "client-injected",
		"claude_session_id": "client-injected",
	}
	if got := ResumeID(metadata); got != "server-owned-1" {
		t.Fatalf("ResumeID = %q, want the server-owned native_session_id only", got)
	}
	if got := ResumeID(nil); got != "" {
		t.Fatalf("ResumeID(nil) = %q, want empty", got)
	}
}

func TestWrapForwardsMappedEventsAndClosesWithNativeStream(t *testing.T) {
	native := newFakeNative()
	session := Wrap(native, WrapOptions{Adapter: "codex"})
	native.src <- events.Event{Kind: events.EventText, Text: "hi"}
	native.src <- events.Event{Kind: events.EventFinish, FinishReason: "end_turn"}
	close(native.src)

	first := <-session.Events()
	if first.Type != runtime.EventText || first.Text != "hi" {
		t.Fatalf("first event = %#v", first)
	}
	second := <-session.Events()
	if second.Type != runtime.EventFinish || second.FinishReason != "end_turn" {
		t.Fatalf("second event = %#v", second)
	}
	if _, open := <-session.Events(); open {
		t.Fatal("runtime stream must close when the native stream closes")
	}

	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if native.aborts != 1 || native.closes != 1 {
		t.Fatalf("aborts=%d closes=%d", native.aborts, native.closes)
	}
}

// blockingNative blocks in Send until its context ends, so a concurrent
// second Send must wait on the wrapper's mutex instead of racing into the
// native session.
type blockingNative struct {
	fakeNative
	inSend chan struct{}
}

func (b *blockingNative) Send(ctx context.Context, input events.Input) error {
	b.inSend <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// TestSendIsSerialized fixes the claudecode drift: the pre-bridge claudecode
// wrapper had no mutex, so concurrent Sends reached the native session
// together. One turn at a time is the runtime contract.
func TestSendIsSerialized(t *testing.T) {
	native := &blockingNative{fakeNative: fakeNative{src: make(chan events.Event)}, inSend: make(chan struct{}, 2)}
	wrapped := Wrap(native, WrapOptions{Adapter: "claudecode"})

	// Deliver a first turn that blocks inside the native session until its
	// (short-lived) context ends.
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		first <- wrapped.Send(ctx, runtime.Input{})
	}()
	<-native.inSend

	// A second Send must not reach the native session while the first is in
	// flight; it proceeds only after the first turn releases the mutex.
	second := make(chan error, 1)
	go func() { second <- wrapped.Send(context.Background(), runtime.Input{}) }()
	select {
	case <-native.inSend:
		t.Fatal("second Send reached the native session while a turn was active")
	case <-time.After(50 * time.Millisecond):
	}

	// Once the first turn's context expires, the queued second Send runs.
	if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Send err = %v, want deadline exceeded", err)
	}
	select {
	case <-native.inSend:
	case <-time.After(2 * time.Second):
		t.Fatal("second Send never reached the native session after the first turn ended")
	}
}

// deadlineNative is a fake native session whose turn never completes on its
// own; only Abort produces a terminal.
type deadlineNative struct {
	fakeNative
	aborted chan struct{}
	onAbort func()
}

func newDeadlineNative() *deadlineNative {
	return &deadlineNative{fakeNative: fakeNative{src: make(chan events.Event, 8)}, aborted: make(chan struct{}, 4)}
}

func (d *deadlineNative) Abort(ctx context.Context) error {
	d.fakeNative.Abort(ctx)
	if d.onAbort != nil {
		d.onAbort()
	}
	d.aborted <- struct{}{}
	return nil
}

// TestTurnDeadlineSettlesToFinishTimeout_OF09b pins the worker-boundary
// enforcement: when the deadline fires, the watcher aborts the native turn;
// once the native terminal settles, the wrapper emits the synthesized
// finish(timeout) INSTEAD of the native terminal (which is only the abort's
// confirmation).
func TestTurnDeadlineSettlesToFinishTimeout_OF09b(t *testing.T) {
	native := newDeadlineNative()
	native.onAbort = func() { native.src <- events.Event{Kind: events.EventFinish, FinishReason: "cancelled"} }
	session := Wrap(native, WrapOptions{Adapter: "codex", TurnTimeout: 50 * time.Millisecond})

	if err := session.Send(context.Background(), runtime.Input{Messages: []runtime.Message{{Role: "user", Content: "work"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-native.aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline watcher never aborted the native turn")
	}
	ev := <-session.Events()
	if ev.Type != runtime.EventFinish || ev.FinishReason != runtime.FinishReasonTimeout {
		t.Fatalf("terminal event = %#v, want synthesized finish(timeout)", ev)
	}
}

// TestTurnDeadlineUnsettledEmitsSentinel_OF09b pins the unsettled path: when
// the deadline abort produces no native terminal within the settle bound, the
// wrapper emits runtime.TurnDeadlineExceeded — the API records
// RunOutcomeUnknown for it.
func TestTurnDeadlineUnsettledEmitsSentinel_OF09b(t *testing.T) {
	native := newDeadlineNative() // Abort produces no terminal
	session := Wrap(native, WrapOptions{Adapter: "codex", TurnTimeout: 30 * time.Millisecond})
	session.settleBound = 50 * time.Millisecond // set before Send: no watcher is running yet

	if err := session.Send(context.Background(), runtime.Input{Messages: []runtime.Message{{Role: "user", Content: "work"}}}); err != nil {
		t.Fatal(err)
	}
	ev := <-session.Events()
	if ev.Type != runtime.EventError || ev.Error != runtime.TurnDeadlineExceeded {
		t.Fatalf("event = %#v, want sentinel error %q", ev, runtime.TurnDeadlineExceeded)
	}
}

// TestTurnDeadlineNotFiredLeavesTerminalIntact_OF09b guards the happy path:
// a turn that finishes before the deadline must forward its native terminal
// untouched and never run an abort.
func TestTurnDeadlineNotFiredLeavesTerminalIntact_OF09b(t *testing.T) {
	native := newDeadlineNative()
	session := Wrap(native, WrapOptions{Adapter: "codex", TurnTimeout: 5 * time.Second})

	if err := session.Send(context.Background(), runtime.Input{Messages: []runtime.Message{{Role: "user", Content: "work"}}}); err != nil {
		t.Fatal(err)
	}
	native.src <- events.Event{Kind: events.EventText, Text: "answer"}
	native.src <- events.Event{Kind: events.EventFinish, FinishReason: "end_turn"}

	first := <-session.Events()
	if first.Type != runtime.EventText || first.Text != "answer" {
		t.Fatalf("first event = %#v", first)
	}
	second := <-session.Events()
	if second.Type != runtime.EventFinish || second.FinishReason != "end_turn" {
		t.Fatalf("terminal = %#v, want native end_turn forwarded intact", second)
	}
	select {
	case <-native.aborted:
		t.Fatal("abort ran although the turn finished in time")
	default:
	}
}

// TestTurnDeadlineNoTimeoutMeansNoWatcher_OF09b: TurnTimeout=0 disables the
// watcher entirely — a hanging turn produces no synthesized events.
func TestTurnDeadlineNoTimeoutMeansNoWatcher_OF09b(t *testing.T) {
	native := newDeadlineNative()
	session := Wrap(native, WrapOptions{Adapter: "codex", TurnTimeout: 0})

	if err := session.Send(context.Background(), runtime.Input{Messages: []runtime.Message{{Role: "user", Content: "work"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-session.Events():
		t.Fatalf("no event expected without a deadline, got %#v", ev)
	case <-native.aborted:
		t.Fatal("abort ran without a configured deadline")
	case <-time.After(100 * time.Millisecond):
	}
}
