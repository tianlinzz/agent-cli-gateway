package worker

import (
	"path/filepath"
	"testing"
)

func TestResolveAgentHomeDirSessionPolicyUsesEphemeralHome(t *testing.T) {
	sessionDir := filepath.Join("gateway-run", "sessions", "abc123")
	for _, policy := range []string{"", "session"} {
		got, err := resolveAgentHomeDir(policy, "/srv/workspaces", "/srv/workspaces/alice/proj", sessionDir)
		if err != nil {
			t.Fatalf("policy %q: %v", policy, err)
		}
		if want := filepath.Join(sessionDir, "agent-home"); got != want {
			t.Fatalf("policy %q: agent home = %q, want %q", policy, got, want)
		}
	}
}

func TestResolveAgentHomeDirWorkspacePolicyKeysHomeByWorkspace(t *testing.T) {
	const root = "/srv/workspaces"
	got, err := resolveAgentHomeDir("workspace", root, filepath.Join(root, "alice", "proj"), "/runtime/sessions/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".agent-homes", "alice", "proj"); got != want {
		t.Fatalf("agent home = %q, want %q (one persistent home per owner workspace)", got, want)
	}
}

func TestResolveAgentHomeDirWorkspacePolicyRejectsEscapingWorkspaceDir(t *testing.T) {
	for _, wsDir := range []string{"/srv/elsewhere", "/srv", "/srv/workspaces/../../etc"} {
		if _, err := resolveAgentHomeDir("workspace", "/srv/workspaces", wsDir, "/s"); err == nil {
			t.Fatalf("workspace dir %q must be rejected as outside the root", wsDir)
		}
	}
}

func TestResolveAgentHomeDirRejectsUnknownPolicy(t *testing.T) {
	if _, err := resolveAgentHomeDir("global", "/srv/workspaces", "/srv/workspaces/a", "/s"); err == nil {
		t.Fatal("unknown agent_home_policy must be rejected")
	}
}
