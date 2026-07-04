package server

import (
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

type errFoo string

func (e errFoo) Error() string { return string(e) }
