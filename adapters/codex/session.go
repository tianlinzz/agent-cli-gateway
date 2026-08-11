package codex

import (
	"context"
	"fmt"
	"strings"
	"sync"

	native "github.com/tianlinzz/agent-cli-gateway/agent/codex"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

type session struct {
	native nativeSession
	events chan runtime.Event
	mu     sync.Mutex
	resume bool
	sent   bool
}

func wrapSession(nativeSession nativeSession) *session {
	return wrapSessionWithResume(nativeSession, false)
}

func wrapSessionWithResume(nativeSession nativeSession, resume bool) *session {
	s := &session{native: nativeSession, events: make(chan runtime.Event, 64), resume: resume}
	go func() {
		defer close(s.events)
		for event := range nativeSession.Events() {
			s.events <- mapEvent(event)
		}
	}()
	return s
}

func (s *session) Send(ctx context.Context, input runtime.Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prompt := promptFromRuntime(input, s.resume || s.sent)
	if err := s.native.Send(ctx, native.Input{Prompt: prompt}); err != nil {
		return err
	}
	s.sent = true
	return nil
}

func promptFromRuntime(input runtime.Input, resume bool) string {
	var users, others []string
	for _, message := range input.Messages {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		switch message.Role {
		case "user":
			users = append(users, content)
		case "system":
			others = append(others, "System instructions:\n"+content)
		case "assistant":
			others = append(others, "Assistant:\n"+content)
		case "tool":
			others = append(others, "Tool result:\n"+content)
		default:
			others = append(others, content)
		}
	}
	if resume {
		if len(users) == 0 {
			return ""
		}
		return users[len(users)-1]
	}
	if len(others) == 0 {
		return strings.Join(users, "\n\n")
	}
	for _, user := range users {
		others = append(others, "User:\n"+user)
	}
	return strings.Join(others, "\n\n")
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
			mapped.Error = "codex: native execution failed"
		}
	case native.EventFinish:
		mapped.Type, mapped.FinishReason = runtime.EventFinish, event.FinishReason
	case native.EventNativeSession:
		mapped.Type = runtime.EventNativeSession
	default:
		mapped.Type, mapped.Error = runtime.EventError, fmt.Sprintf("codex: unknown native event %q", event.Kind)
	}
	return mapped
}

func mapTool(tool *native.ToolCall) *runtime.ToolCall {
	if tool == nil {
		return nil
	}
	return &runtime.ToolCall{ID: tool.ID, Name: tool.Name, Arguments: tool.Arguments, Result: tool.Result, IsError: tool.IsError}
}

func (s *session) Events() <-chan runtime.Event    { return s.events }
func (s *session) Abort(ctx context.Context) error { return s.native.Abort(ctx) }
func (s *session) Close(ctx context.Context) error { return s.native.Close(ctx) }
