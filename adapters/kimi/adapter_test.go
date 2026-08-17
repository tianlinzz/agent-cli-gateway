package kimi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/adapters/internal/bridge"
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

// TestNewConsumesTypedConfigDirectly_OF08 guards the typed-config contract:
// the worker hands runtime.AdapterConfig straight to the factory — no
// environment-variable roundtrip. This test fails if optionsFromConfig goes
// back to reading env-var roundtrip variables.
func TestNewConsumesTypedConfigDirectly_OF08(t *testing.T) {
	opts := optionsFromConfig(runtime.AdapterConfig{
		Execution: runtime.AgentExecutionConfig{
			Command:            []string{"/opt/tools/kimi"},
			DefaultModel:       "kimi-k3",
			Permission:         "deny",
			TurnTimeout:        90 * time.Second,
			Env:                map[string]string{"B": "2", "A": "1"},
			InjectSystemPrompt: true,
		},
		WorkspaceDir: "/ws",
	})
	if !slicesEqual(opts.Command, []string{"/opt/tools/kimi"}) || opts.Model != "kimi-k3" || opts.Permission != "deny" || opts.TurnTimeout != 90*time.Second || !opts.InjectSystemPrompt {
		t.Fatalf("options = %#v", opts)
	}
	if opts.WorkDir != "/ws" {
		t.Fatalf("placement not mapped: %#v", opts)
	}
	if len(opts.Env) != 2 || opts.Env[0] != "A=1" || opts.Env[1] != "B=2" {
		t.Fatalf("env = %#v, want sorted [A=1 B=2]", opts.Env)
	}
}

// TestStartPassesArgvVerbatim_OF14 pins the argv contract: a configured
// command flows to the native launcher verbatim — a path containing spaces
// stays a single token and is never re-tokenized by whitespace.
func TestStartPassesArgvVerbatim_OF14(t *testing.T) {
	capture := &captureStarter{session: newFakeSession("native-1")}
	adapter := newAdapter(Options{Command: []string{"/opt/agent tools/kimi", "--quiet"}}, capture.Start)
	if _, err := adapter.Start(context.Background(), runtime.StartRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/agent tools/kimi", "--quiet"}
	if !slicesEqual(capture.options.Command, want) {
		t.Fatalf("command = %#v, want %#v (argv must flow verbatim)", capture.options.Command, want)
	}
}

func TestAdapterMapsTrustedOptions(t *testing.T) {
	capture := &captureStarter{session: newFakeSession("native-1")}
	adapter := newAdapter(Options{
		Command:     []string{"kimi", "--debug"},
		Env:         []string{"KIMI_API_KEY=secret"},
		WorkDir:     "/workspace",
		Model:       "kimi-k2",
		Mode:        "plan",
		TurnTimeout: 30 * time.Second,
		Permission:  "deny",
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
	if capture.options.ResumeID != "resume-1" || capture.options.WorkDir != "/workspace" || capture.options.Model != "kimi-k2" || capture.options.Mode != "plan" || capture.options.Permission != "deny" {
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
	if err := bridge.Wrap(freshNative, bridge.WrapOptions{Adapter: "kimi", InjectSystemPrompt: false}).Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if freshNative.input.Prompt != "second" {
		t.Fatalf("fresh prompt = %q, want only latest user message", freshNative.input.Prompt)
	}

	resumeNative := newFakeSession("native-1")
	if err := bridge.Wrap(resumeNative, bridge.WrapOptions{Adapter: "kimi", InjectSystemPrompt: false}).Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if resumeNative.input.Prompt != "second" {
		t.Fatalf("resume prompt = %q, want only latest user message", resumeNative.input.Prompt)
	}
}

func TestSessionInjectsSystemPromptWhenEnabled(t *testing.T) {
	nativeSession := newFakeSession("native-1")
	input := runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "be exact"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "tool", Content: "tool result"},
		{Role: "user", Content: "second"},
	}}
	if err := bridge.Wrap(nativeSession, bridge.WrapOptions{Adapter: "kimi", InjectSystemPrompt: true}).Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if want := "be exact\n\nsecond"; nativeSession.input.Prompt != want {
		t.Fatalf("prompt = %q, want %q (system prepended, assistant/tool ignored)", nativeSession.input.Prompt, want)
	}
}

func TestSessionMapsEveryNativeEventAndDelegatesLifecycle(t *testing.T) {
	nativeSession := newFakeSession("native-1")
	session := bridge.Wrap(nativeSession, bridge.WrapOptions{Adapter: "kimi", InjectSystemPrompt: false})
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
	session bridge.NativeSession
}

func (s *captureStarter) Start(_ context.Context, options native.Options) (bridge.NativeSession, error) {
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

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
