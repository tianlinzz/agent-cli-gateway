package kimi

import (
	"context"
	"strings"
	"testing"
	"time"

	native "github.com/tianlinzz/agent-cli-gateway/agent/kimi"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestDescribeDeclaresPersistentProcess(t *testing.T) {
	descriptor, err := newAdapter(Options{}, nil).Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ModelID != "kimi" || descriptor.LifecycleMode != runtime.LifecyclePersistentProcess {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	if !descriptor.Capabilities.Streaming || !descriptor.Capabilities.ToolCalls || !descriptor.Capabilities.Resume || !descriptor.Capabilities.MultiTurn {
		t.Fatalf("capabilities = %#v", descriptor.Capabilities)
	}
}

func TestAdapterMapsTrustedOptions(t *testing.T) {
	capture := &captureStarter{session: newFakeSession("native-1")}
	adapter := newAdapter(Options{
		Command:    "kimi --debug",
		Env:        []string{"KIMI_API_KEY=secret"},
		WorkDir:    "/workspace",
		Model:      "kimi-k2",
		Mode:       "plan",
		Timeout:    30 * time.Second,
		Permission: "deny",
	}, capture.Start)

	_, err := adapter.Start(context.Background(), runtime.StartRequest{
		Metadata: map[string]string{"native_session_id": "resume-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(capture.options.Command, " "); got != "kimi --debug" {
		t.Fatalf("command = %q", got)
	}
	if capture.options.ResumeID != "resume-1" || capture.options.WorkDir != "/workspace" || capture.options.Model != "kimi-k2" || capture.options.Mode != "plan" || capture.options.Permission != "deny" || capture.options.Timeout != 30*time.Second {
		t.Fatalf("options = %#v", capture.options)
	}
	if len(capture.options.Env) != 1 || capture.options.Env[0] != "KIMI_API_KEY=secret" {
		t.Fatalf("options = %#v", capture.options)
	}

}

func TestSessionSendsOnlyLatestUserMessageToNativeAgent(t *testing.T) {
	input := runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "<skills><skill>f1-web-admin</skill></skills>"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "tool", Content: "f1-web tool result"},
		{Role: "user", Content: "second"},
	}}

	freshNative := newFakeSession("")
	if err := wrapSession(freshNative).Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if freshNative.input.Prompt != "second" {
		t.Fatalf("fresh prompt = %q, want only latest user message", freshNative.input.Prompt)
	}

	resumeNative := newFakeSession("native-1")
	if err := wrapSession(resumeNative).Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if resumeNative.input.Prompt != "second" {
		t.Fatalf("resume prompt = %q, want only latest user message", resumeNative.input.Prompt)
	}
}

func TestSessionMapsEveryNativeEventAndDelegatesLifecycle(t *testing.T) {
	nativeSession := newFakeSession("native-1")
	session := wrapSession(nativeSession)
	nativeSession.events <- native.Event{Kind: native.EventText, Text: "text"}
	nativeSession.events <- native.Event{Kind: native.EventReasoning, Reasoning: &native.Reasoning{ID: "reason-1", Text: "thought"}}
	nativeSession.events <- native.Event{Kind: native.EventToolUse, Tool: &native.ToolCall{ID: "tool-1", Name: "Shell", Arguments: map[string]any{"command": "pwd"}}}
	nativeSession.events <- native.Event{Kind: native.EventToolResult, Tool: &native.ToolCall{ID: "tool-1", Result: "/workspace", IsError: false}}
	nativeSession.events <- native.Event{Kind: native.EventPermission, Permission: &native.PermissionRequest{ID: "perm-1", Action: "Shell", Detail: "pwd"}}
	nativeSession.events <- native.Event{Kind: native.EventUsage, Usage: &native.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}
	nativeSession.events <- native.Event{Kind: native.EventNativeSession, NativeSessionID: "native-1"}
	nativeSession.events <- native.Event{Kind: native.EventFinish, FinishReason: "end_turn", NativeSessionID: "native-1"}
	nativeSession.events <- native.Event{Kind: native.EventError, Err: context.Canceled}
	close(nativeSession.events)

	want := []runtime.EventType{
		runtime.EventText, runtime.EventReasoning, runtime.EventToolUse, runtime.EventToolResult,
		runtime.EventPermission, runtime.EventUsage, runtime.EventNativeSession,
		runtime.EventFinish, runtime.EventError,
	}
	for i, eventType := range want {
		event, ok := <-session.Events()
		if !ok {
			t.Fatalf("event stream closed at %d", i)
		}
		if event.Type != eventType {
			t.Fatalf("event %d type = %q, want %q", i, event.Type, eventType)
		}
		if eventType == runtime.EventFinish && (event.FinishReason != "end_turn" || event.NativeSessionID != "native-1") {
			t.Fatalf("finish = %#v", event)
		}
	}
	if _, ok := <-session.Events(); ok {
		t.Fatal("runtime event stream remains open")
	}

	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if nativeSession.aborts != 1 || nativeSession.closes != 1 {
		t.Fatalf("aborts=%d closes=%d", nativeSession.aborts, nativeSession.closes)
	}
}

func TestRegistryContainsKimi(t *testing.T) {
	for _, name := range runtime.List() {
		if name == "kimi" {
			return
		}
	}
	t.Fatal("kimi adapter is not registered")
}

type captureStarter struct {
	options native.Options
	session nativeSession
}

func (s *captureStarter) Start(_ context.Context, options native.Options) (nativeSession, error) {
	s.options = options
	return s.session, nil
}

type fakeSession struct {
	id     string
	events chan native.Event
	input  native.Input
	aborts int
	closes int
}

func newFakeSession(id string) *fakeSession {
	return &fakeSession{id: id, events: make(chan native.Event, 16)}
}

func (s *fakeSession) Send(_ context.Context, input native.Input) error { s.input = input; return nil }
func (s *fakeSession) Events() <-chan native.Event                      { return s.events }
func (s *fakeSession) Abort(context.Context) error                      { s.aborts++; return nil }
func (s *fakeSession) Close(context.Context) error                      { s.closes++; return nil }
func (s *fakeSession) NativeSessionID() string                          { return s.id }
