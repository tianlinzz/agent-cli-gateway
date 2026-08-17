// Package testcorpus holds the cross-adapter parity fixtures required by the
// optimization roadmap (§3.4: extract shared adapter helpers carefully —
// "先补 table-driven parity 测试"). Every adapter package runs the SAME corpus
// through its own bridge-wrapped session, so any behavioral drift between the
// adapters (or a regression in the shared bridge) fails all three parity
// tests identically. The package is test-support only: production code never
// imports it.
package testcorpus

import (
	"github.com/tianlinzz/agent-cli-gateway/agent/events"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// EventCase is one native-event → runtime-event mapping fixture. Want is a
// function because synthesized error payloads carry the adapter name.
type EventCase struct {
	Name   string
	Native events.Event
	Want   func(adapter string) runtime.Event
}

// EventCases covers every native event kind plus the two synthesized-error
// shapes. Values mirror the per-adapter tests; the corpus exists to prove the
// three adapters produce IDENTICAL output for IDENTICAL input.
var EventCases = []EventCase{
	{
		Name:   "text",
		Native: events.Event{Kind: events.EventText, Text: "hello", NativeSessionID: "native-1"},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventText, Text: "hello", NativeSessionID: "native-1"}
		},
	},
	{
		Name:   "reasoning",
		Native: events.Event{Kind: events.EventReasoning, Reasoning: &events.Reasoning{ID: "r1", Text: "summary"}},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventReasoning, Reasoning: &runtime.Reasoning{ID: "r1", Text: "summary"}}
		},
	},
	{
		Name:   "tool_use",
		Native: events.Event{Kind: events.EventToolUse, Tool: &events.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{"command": "pwd"}}},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{"command": "pwd"}}}
		},
	},
	{
		Name:   "tool_result",
		Native: events.Event{Kind: events.EventToolResult, Tool: &events.ToolCall{ID: "t1", Result: "/workspace", IsError: true}},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{ID: "t1", Result: "/workspace", IsError: true}}
		},
	},
	{
		Name:   "permission",
		Native: events.Event{Kind: events.EventPermission, Permission: &events.PermissionRequest{ID: "p1", Action: "Bash", Detail: "pwd"}},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventPermission, Permission: &runtime.PermissionRequest{ID: "p1", Action: "Bash", Detail: "pwd"}}
		},
	},
	{
		Name:   "usage",
		Native: events.Event{Kind: events.EventUsage, Usage: &events.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}
		},
	},
	{
		Name:   "finish",
		Native: events.Event{Kind: events.EventFinish, FinishReason: "end_turn", NativeSessionID: "native-1"},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn", NativeSessionID: "native-1"}
		},
	},
	{
		Name:   "native_session",
		Native: events.Event{Kind: events.EventNativeSession, NativeSessionID: "native-1"},
		Want: func(string) runtime.Event {
			return runtime.Event{Type: runtime.EventNativeSession, NativeSessionID: "native-1"}
		},
	},
	{
		Name:   "error without cause is adapter-prefixed",
		Native: events.Event{Kind: events.EventError},
		Want: func(adapter string) runtime.Event {
			return runtime.Event{Type: runtime.EventError, Error: adapter + ": native execution failed"}
		},
	},
	{
		Name:   "unknown kind is an adapter-prefixed error",
		Native: events.Event{Kind: events.EventKind("mystery")},
		Want: func(adapter string) runtime.Event {
			return runtime.Event{Type: runtime.EventError, Error: adapter + `: unknown native event "mystery"`}
		},
	},
}

// PromptCase is one canonical-input → native-prompt fixture.
type PromptCase struct {
	Name         string
	Input        runtime.Input
	InjectSystem bool
	Want         string
}

// PromptCases pins the prompt-assembly contract: only the latest user message
// flows to the native agent unless system-prompt injection is enabled.
var PromptCases = []PromptCase{
	{
		Name: "latest user message only",
		Input: runtime.Input{Messages: []runtime.Message{
			{Role: "system", Content: "be exact"},
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "previous answer"},
			{Role: "tool", Content: "tool result"},
			{Role: "user", Content: "second"},
		}},
		Want: "second",
	},
	{
		Name: "system prepended when injection enabled",
		Input: runtime.Input{Messages: []runtime.Message{
			{Role: "system", Content: "be exact"},
			{Role: "system", Content: "   "},
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "previous answer"},
			{Role: "user", Content: "second"},
		}},
		InjectSystem: true,
		Want:         "be exact\n\nsecond",
	},
	{
		Name: "empty input yields empty prompt",
		Input: runtime.Input{Messages: []runtime.Message{
			{Role: "assistant", Content: "no user turn"},
		}},
		InjectSystem: true,
		Want:         "",
	},
}

// AdapterName identifies the adapter under test in per-adapter parity runs;
// it is the same string passed to bridge.WrapOptions.Adapter.
type AdapterName = string
