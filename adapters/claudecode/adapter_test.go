package claudecode

import (
	"context"
	"testing"

	native "github.com/tianlinzz/agent-cli-gateway/agent/claudecode"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestDescribeDeclaresClaudeCapabilities(t *testing.T) {
	adapter := newAdapter(Options{}, nil)
	descriptor, err := adapter.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ModelID != "claude-code" || descriptor.LifecycleMode != runtime.LifecyclePersistentProcess {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	if !descriptor.Capabilities.Streaming || !descriptor.Capabilities.Permission || !descriptor.Capabilities.Resume {
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
			Command:            []string{"/opt/tools/claude"},
			DefaultModel:       "sonnet",
			Permission:         "deny",
			Env:                map[string]string{"B": "2", "A": "1"},
			InjectSystemPrompt: true,
		},
		WorkspaceDir: "/ws",
	})
	if !slicesEqual(opts.Command, []string{"/opt/tools/claude"}) || opts.Model != "sonnet" || opts.Permission != "deny" || !opts.InjectSystemPrompt {
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
	capture := &captureStarter{session: newFakeNativeSession()}
	adapter := newAdapter(Options{Command: []string{"/opt/agent tools/claude", "--quiet"}}, capture.Start)
	if _, err := adapter.Start(context.Background(), runtime.StartRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/agent tools/claude", "--quiet"}
	if !slicesEqual(capture.options.Command, want) {
		t.Fatalf("command = %#v, want %#v (argv must flow verbatim)", capture.options.Command, want)
	}
}

func TestStartMapsTrustedOptionsAndNativeResumeID(t *testing.T) {
	capture := &captureStarter{session: newFakeNativeSession()}
	adapter := newAdapter(Options{
		Command: []string{"claude", "--debug"},
		WorkDir: "/workspace",
		Model:   "haiku",
		Mode:    "acceptEdits",
	}, capture.Start)

	_, err := adapter.Start(context.Background(), runtime.StartRequest{
		Metadata: map[string]string{"native_session_id": "native-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := capture.options.Command; len(got) != 2 || got[0] != "claude" || got[1] != "--debug" {
		t.Fatalf("native command = %#v", got)
	}
	if capture.options.WorkDir != "/workspace" || capture.options.Model != "haiku" || capture.options.ResumeID != "native-1" {
		t.Fatalf("native options = %#v", capture.options)
	}
}

func TestSessionMapsNativeEventsToRuntime(t *testing.T) {
	nativeSession := newFakeNativeSession()
	session := wrapSession(nativeSession, false)
	nativeSession.events <- native.Event{Kind: native.EventText, Text: "hello"}
	nativeSession.events <- native.Event{Kind: native.EventReasoning, Reasoning: &native.Reasoning{ID: "reason-1", Text: "safe summary"}}
	nativeSession.events <- native.Event{Kind: native.EventToolUse, Tool: &native.ToolCall{ID: "tool-1", Name: "Read", Arguments: map[string]any{"file_path": "a.go"}}}
	nativeSession.events <- native.Event{Kind: native.EventPermission, Permission: &native.PermissionRequest{ID: "perm-1", Action: "Bash", Detail: "pwd"}}
	nativeSession.events <- native.Event{Kind: native.EventUsage, Usage: &native.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}
	nativeSession.events <- native.Event{Kind: native.EventFinish, FinishReason: "end_turn", NativeSessionID: "native-1"}
	close(nativeSession.events)

	want := []runtime.EventType{runtime.EventText, runtime.EventReasoning, runtime.EventToolUse, runtime.EventPermission, runtime.EventUsage, runtime.EventFinish}
	for i, eventType := range want {
		event, ok := <-session.Events()
		if !ok {
			t.Fatalf("event stream closed at %d", i)
		}
		if event.Type != eventType {
			t.Fatalf("event %d type = %q, want %q", i, event.Type, eventType)
		}
		if eventType == runtime.EventFinish && event.NativeSessionID != "native-1" {
			t.Fatalf("finish = %#v", event)
		}
	}
	if _, ok := <-session.Events(); ok {
		t.Fatal("runtime event stream remains open")
	}
}

func TestSessionConvertsLastUserMessageAndDelegatesLifecycle(t *testing.T) {
	nativeSession := newFakeNativeSession()
	session := wrapSession(nativeSession, false)
	err := session.Send(context.Background(), runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "second"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if nativeSession.input.Prompt != "second" {
		t.Fatalf("native prompt = %q", nativeSession.input.Prompt)
	}
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if nativeSession.abortCalls != 1 || nativeSession.closeCalls != 1 {
		t.Fatalf("abort=%d close=%d", nativeSession.abortCalls, nativeSession.closeCalls)
	}
}

func TestSessionInjectsSystemPromptWhenEnabled(t *testing.T) {
	nativeSession := newFakeNativeSession()
	session := wrapSession(nativeSession, true)
	err := session.Send(context.Background(), runtime.Input{Messages: []runtime.Message{
		{Role: "system", Content: "be exact"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "second"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "be exact\n\nsecond"; nativeSession.input.Prompt != want {
		t.Fatalf("native prompt = %q, want %q (system prepended)", nativeSession.input.Prompt, want)
	}
}

func TestRegistryContainsClaudeCode(t *testing.T) {
	for _, name := range runtime.List() {
		if name == "claude-code" {
			return
		}
	}
	t.Fatal("claude-code adapter is not registered")
}

type captureStarter struct {
	options native.Options
	session nativeSession
}

func (s *captureStarter) Start(_ context.Context, options native.Options) (nativeSession, error) {
	s.options = options
	return s.session, nil
}

type fakeNativeSession struct {
	events     chan native.Event
	input      native.Input
	abortCalls int
	closeCalls int
}

func newFakeNativeSession() *fakeNativeSession {
	return &fakeNativeSession{events: make(chan native.Event, 8)}
}

func (s *fakeNativeSession) Send(_ context.Context, input native.Input) error {
	s.input = input
	return nil
}
func (s *fakeNativeSession) Events() <-chan native.Event { return s.events }
func (s *fakeNativeSession) Abort(context.Context) error {
	s.abortCalls++
	return nil
}
func (s *fakeNativeSession) Close(context.Context) error {
	s.closeCalls++
	return nil
}
func (s *fakeNativeSession) NativeSessionID() string { return "native-1" }

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
