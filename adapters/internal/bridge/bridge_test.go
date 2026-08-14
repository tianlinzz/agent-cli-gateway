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
