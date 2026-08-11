package claudecode

import (
	"context"
	"fmt"
	"strings"

	native "github.com/tianlinzz/agent-cli-gateway/agent/claudecode"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

type session struct {
	native nativeSession
	events chan runtime.Event
}

func wrapSession(nativeSession nativeSession) *session {
	s := &session{native: nativeSession, events: make(chan runtime.Event, 64)}
	go s.forwardEvents()
	return s
}

func (s *session) forwardEvents() {
	defer close(s.events)
	for event := range s.native.Events() {
		s.events <- mapEvent(event)
	}
}

func mapEvent(event native.Event) runtime.Event {
	mapped := runtime.Event{NativeSessionID: event.NativeSessionID}
	switch event.Kind {
	case native.EventText:
		mapped.Type, mapped.Text = runtime.EventText, event.Text
	case native.EventReasoning:
		mapped.Type = runtime.EventReasoning
		if event.Reasoning != nil {
			mapped.Reasoning = &runtime.Reasoning{ID: event.Reasoning.ID, Text: event.Reasoning.Text}
		}
	case native.EventToolUse:
		mapped.Type, mapped.Tool = runtime.EventToolUse, mapTool(event.Tool)
	case native.EventToolResult:
		mapped.Type, mapped.Tool = runtime.EventToolResult, mapTool(event.Tool)
	case native.EventPermission:
		mapped.Type = runtime.EventPermission
		if event.Permission != nil {
			mapped.Permission = &runtime.PermissionRequest{ID: event.Permission.ID, Action: event.Permission.Action, Detail: event.Permission.Detail}
		}
	case native.EventUsage:
		mapped.Type = runtime.EventUsage
		if event.Usage != nil {
			mapped.Usage = &runtime.Usage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens, TotalTokens: event.Usage.TotalTokens}
		}
	case native.EventError:
		mapped.Type = runtime.EventError
		if event.Err != nil {
			mapped.Error = event.Err.Error()
		} else {
			mapped.Error = "claudecode: native execution failed"
		}
	case native.EventFinish:
		mapped.Type, mapped.FinishReason = runtime.EventFinish, event.FinishReason
	case native.EventNativeSession:
		mapped.Type = runtime.EventNativeSession
	default:
		mapped.Type, mapped.Error = runtime.EventError, fmt.Sprintf("claudecode: unknown native event %q", event.Kind)
	}
	return mapped
}

func mapTool(tool *native.ToolCall) *runtime.ToolCall {
	if tool == nil {
		return nil
	}
	return &runtime.ToolCall{
		ID: tool.ID, Name: tool.Name, Arguments: tool.Arguments,
		Result: tool.Result, IsError: tool.IsError,
	}
}

func (s *session) Send(ctx context.Context, input runtime.Input) error {
	return s.native.Send(ctx, native.Input{Prompt: lastUserMessage(input)})
}

func lastUserMessage(input runtime.Input) string {
	var prompt string
	for _, message := range input.Messages {
		if message.Role == "user" && strings.TrimSpace(message.Content) != "" {
			prompt = message.Content
		}
	}
	return prompt
}

func (s *session) Events() <-chan runtime.Event    { return s.events }
func (s *session) Abort(ctx context.Context) error { return s.native.Abort(ctx) }
func (s *session) Close(ctx context.Context) error { return s.native.Close(ctx) }
