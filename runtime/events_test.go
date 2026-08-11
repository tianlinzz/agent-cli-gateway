package runtime

import "testing"

func TestReasoningEventContract(t *testing.T) {
	event := Event{
		Type:      EventReasoning,
		Reasoning: &Reasoning{ID: "reason-1", Text: "checking workspace"},
	}
	if event.Type != EventReasoning {
		t.Fatalf("event type = %q, want %q", event.Type, EventReasoning)
	}
	if event.Reasoning == nil || event.Reasoning.ID != "reason-1" || event.Reasoning.Text != "checking workspace" {
		t.Fatalf("reasoning payload = %#v", event.Reasoning)
	}
}

func TestToolEventContractRemainsNative(t *testing.T) {
	event := Event{Type: EventToolResult, Tool: &ToolCall{ID: "tool-1", Name: "Bash", Result: "ok"}}
	if event.Type != EventToolResult {
		t.Fatalf("event type = %q, want %q", event.Type, EventToolResult)
	}
	if event.Tool == nil || event.Tool.ID != "tool-1" {
		t.Fatalf("tool payload = %#v", event.Tool)
	}
}
