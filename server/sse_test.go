package server

import (
	"encoding/json"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

func TestEventToPayloadText(t *testing.T) {
	event := core.Event{Type: core.EventText, Content: "hello world"}
	payload := eventToPayload(event)
	if payload.Type != "text" {
		t.Errorf("expected type 'text', got %q", payload.Type)
	}
	if payload.Content != "hello world" {
		t.Errorf("expected content 'hello world', got %q", payload.Content)
	}
}

func TestEventToPayloadToolUse(t *testing.T) {
	event := core.Event{Type: core.EventToolUse, ToolName: "Read", ToolInput: "file_path=/x.go"}
	payload := eventToPayload(event)
	if payload.Type != "tool_use" {
		t.Errorf("expected type 'tool_use', got %q", payload.Type)
	}
	if payload.ToolName != "Read" {
		t.Errorf("expected toolName 'Read', got %q", payload.ToolName)
	}
	if payload.ToolInput != "file_path=/x.go" {
		t.Errorf("expected toolInput 'file_path=/x.go', got %q", payload.ToolInput)
	}
}

func TestEventToPayloadResultWithTokens(t *testing.T) {
	input := 100
	output := 50
	event := core.Event{Type: core.EventResult, Done: true, InputTokens: input, OutputTokens: output}
	payload := eventToPayload(event)
	if !payload.Done {
		t.Error("expected Done=true")
	}
	if payload.InputTokens != 100 {
		t.Errorf("expected InputTokens=100, got %d", payload.InputTokens)
	}
	if payload.OutputTokens != 50 {
		t.Errorf("expected OutputTokens=50, got %d", payload.OutputTokens)
	}
}

func TestEventToPayloadError(t *testing.T) {
	event := core.Event{Type: core.EventError, Error: errFoo("agent crashed")}
	payload := eventToPayload(event)
	if payload.Error != "agent crashed" {
		t.Errorf("expected error 'agent crashed', got %q", payload.Error)
	}
}

func TestEventToPayloadMetadata(t *testing.T) {
	event := core.Event{
		Type:     core.EventText,
		Content:  "checking constraints",
		Metadata: map[string]any{"phase": "commentary", "kind": "assistant_commentary"},
	}
	payload := eventToPayload(event)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	metadata, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing from payload: %s", string(data))
	}
	if metadata["phase"] != "commentary" || metadata["kind"] != "assistant_commentary" {
		t.Fatalf("metadata = %#v, want phase/kind", metadata)
	}
}

type errFoo string

func (e errFoo) Error() string { return string(e) }
