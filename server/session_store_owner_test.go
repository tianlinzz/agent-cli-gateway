package server

import (
	"context"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// stubAgentSession is a minimal AgentSession for store-level tests.
type stubAgentSession struct{}

func (stubAgentSession) Send(string, []core.ImageAttachment, []core.FileAttachment) error { return nil }
func (stubAgentSession) RespondPermission(string, core.PermissionResult) error            { return nil }
func (stubAgentSession) Events() <-chan core.Event                                          { return nil }
func (stubAgentSession) CurrentSessionID() string                                           { return "" }
func (stubAgentSession) Alive() bool                                                        { return true }
func (stubAgentSession) Close() error                                                       { return nil }

// stubAgent is a minimal Agent that returns a stubAgentSession.
type stubAgent struct{ name string }

func (a *stubAgent) Name() string { return a.name }
func (a *stubAgent) StartSession(ctx context.Context, id string) (core.AgentSession, error) {
	return stubAgentSession{}, nil
}
func (a *stubAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *stubAgent) Stop() error { return nil }

func TestGetSessionForOwner_RejectsCrossCaller(t *testing.T) {
	store := NewSessionStore(map[string]core.Agent{"stub": &stubAgent{"stub"}})

	// Create a session owned by alice.
	ms, err := store.CreateSession(withOwnerContext(context.Background(), "alice"), CreateSessionRequest{
		Agent: "stub",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// alice can access.
	if _, ok := store.GetSessionForOwner(ms.ID, "alice"); !ok {
		t.Error("alice: expected ok, got not found")
	}
	// bob cannot — must return false (caller distinguishes "not mine" from "absent").
	if _, ok := store.GetSessionForOwner(ms.ID, "bob"); ok {
		t.Error("bob: expected not-found/forbidden, got ok")
	}
}
