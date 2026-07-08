package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCodexSession writes a minimal codex JSONL transcript into sessionsDir.
func writeCodexSession(t *testing.T, sessionsDir, id, cwd string, mtime time.Time) {
	t.Helper()
	// Codex stores sessions under sessions/YYYY/MM/DD/rollout-<timestamp>-<id>.jsonl
	dayDir := filepath.Join(sessionsDir, "2026", "05", "20")
	if err := os.MkdirAll(dayDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dayDir, "rollout-2026-05-20T10-00-00-"+id+".jsonl")
	content := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `"}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n" +
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestListCodexSessions_NoFilterReturnsAll(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions")
	writeCodexSession(t, sessionsDir, "sess-a", "/workspace/f1-web", time.Unix(1750000100, 0))
	writeCodexSession(t, sessionsDir, "sess-b", "/workspace/ibrain", time.Unix(1750000000, 0))

	// Empty workDir → no filter, all sessions returned.
	sessions, err := listCodexSessions("", codexHome)
	if err != nil {
		t.Fatalf("listCodexSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2", len(sessions))
	}
	// Sorted by ModifiedAt descending.
	if sessions[0].ID != "sess-a" {
		t.Errorf("sessions[0].ID = %q, want sess-a", sessions[0].ID)
	}
	// Cwd should be populated.
	if sessions[0].Cwd != "/workspace/f1-web" {
		t.Errorf("sessions[0].Cwd = %q, want /workspace/f1-web", sessions[0].Cwd)
	}
}

func TestListCodexSessions_DotWorkDirReturnsAll(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions")
	writeCodexSession(t, sessionsDir, "sess-a", "/workspace/f1-web", time.Unix(1750000100, 0))
	writeCodexSession(t, sessionsDir, "sess-b", "/workspace/other", time.Unix(1750000000, 0))

	// "." (the gateway-singleton default) → no filter.
	sessions, err := listCodexSessions(".", codexHome)
	if err != nil {
		t.Fatalf("listCodexSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2 (dot workDir should not filter)", len(sessions))
	}
}

func TestListCodexSessions_FilteredByWorkDir(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions")
	writeCodexSession(t, sessionsDir, "sess-a", "/workspace/f1-web", time.Unix(1750000100, 0))
	writeCodexSession(t, sessionsDir, "sess-b", "/workspace/other", time.Unix(1750000000, 0))

	sessions, err := listCodexSessions("/workspace/f1-web", codexHome)
	if err != nil {
		t.Fatalf("listCodexSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1 (filtered)", len(sessions))
	}
	if sessions[0].ID != "sess-a" {
		t.Errorf("sessions[0].ID = %q, want sess-a", sessions[0].ID)
	}
}

func TestListCodexWorkspaces(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions")
	writeCodexSession(t, sessionsDir, "sess-a", "/workspace/f1-web", time.Unix(1750000100, 0))
	writeCodexSession(t, sessionsDir, "sess-b", "/workspace/f1-web", time.Unix(1750000050, 0))
	writeCodexSession(t, sessionsDir, "sess-c", "/workspace/ibrain", time.Unix(1750000000, 0))

	ws, err := listCodexWorkspaces(codexHome)
	if err != nil {
		t.Fatalf("listCodexWorkspaces: %v", err)
	}
	if len(ws) != 2 {
		t.Fatalf("workspaces len = %d, want 2", len(ws))
	}
	// Sorted by LastActive descending → f1-web first.
	if ws[0].Path != "/workspace/f1-web" {
		t.Errorf("workspaces[0].Path = %q, want /workspace/f1-web", ws[0].Path)
	}
	if ws[0].SessionCount != 2 {
		t.Errorf("workspaces[0].SessionCount = %d, want 2", ws[0].SessionCount)
	}
	if !ws[0].LastActive.Equal(time.Unix(1750000100, 0)) {
		t.Errorf("workspaces[0].LastActive = %v, want 1750000100", ws[0].LastActive)
	}
	if ws[1].Path != "/workspace/ibrain" || ws[1].SessionCount != 1 {
		t.Errorf("workspaces[1] = %+v, want ibrain/1", ws[1])
	}
}

func TestListCodexWorkspaces_Empty(t *testing.T) {
	codexHome := t.TempDir()
	ws, err := listCodexWorkspaces(codexHome)
	if err != nil {
		t.Fatalf("listCodexWorkspaces: %v", err)
	}
	if len(ws) != 0 {
		t.Fatalf("workspaces len = %d, want 0", len(ws))
	}
}

func TestGetSessionHistory_MarksCodexCommentarySeparately(t *testing.T) {
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "05", "20")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(sessionsDir, "rollout-2026-05-20T10-00-00-sess-phase.jsonl")
	content := `{"timestamp":"2026-05-20T10:00:00Z","type":"session_meta","payload":{"id":"sess-phase","cwd":"/workspace/f1-web"}}` + "\n" +
		`{"timestamp":"2026-05-20T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n" +
		`{"timestamp":"2026-05-20T10:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"checking constraints"}]}}` + "\n" +
		`{"timestamp":"2026-05-20T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"hi there"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	entries, err := getSessionHistory("sess-phase", codexHome, 0)
	if err != nil {
		t.Fatalf("getSessionHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries len = %d, want 3", len(entries))
	}
	if entries[0].Kind != "user" {
		t.Fatalf("entries[0].Kind = %q, want user", entries[0].Kind)
	}
	if entries[1].Kind != "assistant_commentary" || entries[1].Phase != "commentary" {
		t.Fatalf("entries[1] = %+v, want assistant_commentary/commentary", entries[1])
	}
	if entries[2].Kind != "assistant_final" || entries[2].Phase != "final_answer" {
		t.Fatalf("entries[2] = %+v, want assistant_final/final_answer", entries[2])
	}
}
