package openai

// ExecutionBackend abstraction contract tests. The API layer's only execution
// path is the runtime.ExecutionBackend interface; these tests prove the whole
// HTTP surface runs unchanged against a purely in-process implementation, so
// swapping in a cross-node backend later touches only the backend, never the
// API or adapter contract. (api/openai importing no worker/adapter packages
// is enforced separately by internal/archtest.)

import (
	"context"
	"net/http"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// Compile-time proof: the in-process fake satisfies the same contract as
// worker.LocalExecutionBackend.
var _ runtime.ExecutionBackend = (*fakeBackend)(nil)

// TestExecutionBackendSwapInProcess drives a full non-streaming completion
// through the real HTTP stack with the in-process fake backend: the API layer
// code is exercised unchanged with no worker process, no socket, and no gRPC.
func TestExecutionBackendSwapInProcess(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "pong"})
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if resp.Header.Get("X-Gateway-Session-Id") == "" {
		t.Fatal("response carries no gateway session id header")
	}
	started := backend.StartRequests()
	if len(started) != 1 {
		t.Fatalf("backend saw %d starts, want 1", len(started))
	}
	if started[0].ModelID != "codex" || started[0].WorkspaceID != testWorkspace {
		t.Fatalf("start request = %+v, want model codex in workspace %q", started[0], testWorkspace)
	}
}

// TestWorkerIdentityRecordedOnSessionRecord proves the registration contract
// end to end on the local path: a handle exposing the optional identityHandle
// capability gets its worker/node identity copied onto the session record.
func TestWorkerIdentityRecordedOnSessionRecord(t *testing.T) {
	ts, h, backend := newTestServer(t)
	backend.workerID, backend.nodeID = "worker-1", "node-1"
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "pong"})
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	sessionID := resp.Header.Get("X-Gateway-Session-Id")
	if sessionID == "" {
		t.Fatal("response carries no gateway session id header")
	}
	rec, err := h.store.Get(context.Background(), sessionID, testOwner)
	if err != nil {
		t.Fatalf("get session record: %v", err)
	}
	if rec.WorkerID != "worker-1" || rec.NodeID != "node-1" {
		t.Fatalf("session record identity = (%q, %q), want (%q, %q)", rec.WorkerID, rec.NodeID, "worker-1", "node-1")
	}
}

// TestWorkerIdentityAbsentLeavesRecordEmpty: a backend whose handle does not
// report an identity leaves SessionRecord.WorkerID/NodeID empty (the
// capability is optional, never a request failure).
func TestWorkerIdentityAbsentLeavesRecordEmpty(t *testing.T) {
	ts, h, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "pong"})
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	sessionID := resp.Header.Get("X-Gateway-Session-Id")
	rec, err := h.store.Get(context.Background(), sessionID, testOwner)
	if err != nil {
		t.Fatalf("get session record: %v", err)
	}
	if rec.WorkerID != "" || rec.NodeID != "" {
		t.Fatalf("session record identity = (%q, %q), want empty", rec.WorkerID, rec.NodeID)
	}
}
