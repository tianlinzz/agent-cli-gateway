package codex

import (
	"context"
	"testing"

	native "github.com/tianlinzz/agent-cli-gateway/agent/codex"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestDescribeDeclaresPersistentProcess(t *testing.T) {
	descriptor, err := newAdapter(Options{}, nil).Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ModelID != "codex" || descriptor.LifecycleMode != runtime.LifecyclePersistentProcess || !descriptor.Capabilities.Resume {
		t.Fatalf("descriptor = %#v", descriptor)
	}
}

// TestNewConsumesTypedConfigDirectly_OF08 guards the typed-config contract:
// the worker hands runtime.AdapterConfig straight to the factory — no
// environment-variable roundtrip. This test fails if optionsFromConfig goes
// back to reading env-var roundtrip variables.
func TestNewConsumesTypedConfigDirectly_OF08(t *testing.T) {
	opts := optionsFromConfig(runtime.AdapterConfig{
		Execution: runtime.AgentExecutionConfig{
			Command:            "/opt/tools/codex",
			DefaultModel:       "gpt-5",
			Permission:         "deny",
			Env:                map[string]string{"B": "2", "A": "1"},
			InjectSystemPrompt: true,
		},
		WorkspaceDir: "/ws",
		AgentHome:    "/home-agent",
	})
	if opts.Command != "/opt/tools/codex" || opts.Model != "gpt-5" || opts.Permission != "deny" || !opts.InjectSystemPrompt {
		t.Fatalf("options = %#v", opts)
	}
	if opts.WorkDir != "/ws" || opts.CodexHome != "/home-agent" {
		t.Fatalf("placement not mapped: %#v", opts)
	}
	// Env map renders deterministically (sorted K=V).
	if len(opts.Env) != 2 || opts.Env[0] != "A=1" || opts.Env[1] != "B=2" {
		t.Fatalf("env = %#v, want sorted [A=1 B=2]", opts.Env)
	}
}

func TestStartMapsCodexHomeAndResumeMetadata(t *testing.T) {
	capture := &captureStarter{session: newFakeSession("")}
	adapter := newAdapter(Options{Command: "codex --quiet", WorkDir: "/workspace", CodexHome: "/agent-home", Model: "gpt-5", Permission: "deny"}, capture.Start)
	_, err := adapter.Start(context.Background(), runtime.StartRequest{Metadata: map[string]string{"native_session_id": "thread-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if capture.options.ResumeID != "thread-1" || capture.options.WorkDir != "/workspace" || capture.options.Model != "gpt-5" || capture.options.Permission != "deny" {
		t.Fatalf("options = %#v", capture.options)
	}
	if !containsValue(capture.options.Env, "CODEX_HOME=/agent-home") {
		t.Fatalf("env = %#v", capture.options.Env)
	}
}

func TestSessionSendsOnlyLatestUserMessageToNativeAgent(t *testing.T) {
	freshNative := newFakeSession("")
	fresh := wrapSession(freshNative, false)
	input := runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "<skills><skill>f1-web-admin</skill></skills>"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "tool", Content: "f1-web tool result"},
		{Role: "user", Content: "second"},
	}}
	if err := fresh.Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if freshNative.input.Prompt != "second" {
		t.Fatalf("fresh prompt = %q, want only latest user message", freshNative.input.Prompt)
	}

	resumeNative := newFakeSession("thread-1")
	resume := wrapSession(resumeNative, false)
	if err := resume.Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if resumeNative.input.Prompt != "second" {
		t.Fatalf("resume prompt = %q, want only latest user message", resumeNative.input.Prompt)
	}
}

func TestSessionInjectsSystemPromptWhenEnabled(t *testing.T) {
	nativeSession := newFakeSession("")
	session := wrapSession(nativeSession, true)
	input := runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "be exact"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "tool", Content: "tool result"},
		{Role: "user", Content: "second"},
	}}
	if err := session.Send(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if want := "be exact\n\nsecond"; nativeSession.input.Prompt != want {
		t.Fatalf("prompt = %q, want %q (system prepended, assistant/tool ignored)", nativeSession.input.Prompt, want)
	}
}

func TestSessionMapsEventsAndDelegatesLifecycle(t *testing.T) {
	nativeSession := newFakeSession("thread-1")
	session := wrapSession(nativeSession, false)
	nativeSession.events <- native.Event{Kind: native.EventText, Text: "text"}
	nativeSession.events <- native.Event{Kind: native.EventReasoning, Reasoning: &native.Reasoning{ID: "reason-1", Text: "reasoning"}}
	nativeSession.events <- native.Event{Kind: native.EventToolUse, Tool: &native.ToolCall{ID: "tool-1", Name: "Bash", Arguments: map[string]any{"command": "pwd"}}}
	nativeSession.events <- native.Event{Kind: native.EventToolResult, Tool: &native.ToolCall{ID: "tool-1", Name: "Bash", Result: "/workspace"}}
	nativeSession.events <- native.Event{Kind: native.EventUsage, Usage: &native.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}
	nativeSession.events <- native.Event{Kind: native.EventFinish, FinishReason: "end_turn", NativeSessionID: "thread-1"}
	close(nativeSession.events)
	for _, want := range []runtime.EventType{runtime.EventText, runtime.EventReasoning, runtime.EventToolUse, runtime.EventToolResult, runtime.EventUsage, runtime.EventFinish} {
		if event := <-session.Events(); event.Type != want {
			t.Fatalf("event type = %q, want %q", event.Type, want)
		}
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
	return &fakeSession{id: id, events: make(chan native.Event, 8)}
}
func (s *fakeSession) Send(_ context.Context, input native.Input) error { s.input = input; return nil }
func (s *fakeSession) Events() <-chan native.Event                      { return s.events }
func (s *fakeSession) Abort(context.Context) error                      { s.aborts++; return nil }
func (s *fakeSession) Close(context.Context) error                      { s.closes++; return nil }
func (s *fakeSession) NativeSessionID() string                          { return s.id }

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
