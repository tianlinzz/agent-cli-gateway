package server

import (
	"context"
	"sync"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// mockAgent implements core.Agent for testing.
type mockAgent struct {
	name     string
	mu       sync.Mutex
	sessions map[string]*mockSession
}

func newMockAgent(name string) *mockAgent {
	return &mockAgent{
		name:     name,
		sessions: make(map[string]*mockSession),
	}
}

func (a *mockAgent) Name() string { return a.name }
func (a *mockAgent) Stop() error  { return nil }

func (a *mockAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	s := &mockSession{
		id:     sessionID,
		events: make(chan core.Event, 100),
		alive:  true,
	}
	a.mu.Lock()
	a.sessions[sessionID] = s
	a.mu.Unlock()
	return s, nil
}

func (a *mockAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

// mockSession implements core.AgentSession for testing.
type mockSession struct {
	id      string
	events  chan core.Event
	mu      sync.Mutex
	alive   bool
	prompts []string
}

func (s *mockSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()

	// Simulate a simple echo response: text + result.
	go func() {
		s.events <- core.Event{Type: core.EventText, Content: "echo: " + prompt}
		s.events <- core.Event{Type: core.EventResult, Done: true, InputTokens: 10, OutputTokens: 5}
	}()
	return nil
}

func (s *mockSession) RespondPermission(requestID string, result core.PermissionResult) error {
	return nil
}

func (s *mockSession) Events() <-chan core.Event { return s.events }
func (s *mockSession) CurrentSessionID() string   { return s.id }
func (s *mockSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}
func (s *mockSession) Close() error {
	s.mu.Lock()
	s.alive = false
	s.mu.Unlock()
	close(s.events)
	return nil
}
