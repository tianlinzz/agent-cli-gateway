package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// ---------------------------------------------------------------------------
// Step-5 integration test: the same OpenAI /v1/chat/completions routes to
// three different adapters by `model` (codex, claude-code, kimi), and
// /v1/models discovers all three. Fake adapters/backend only — no real CLI is
// launched. Assertions are user-visible response behavior, not internal
// session fields.
// ---------------------------------------------------------------------------

// threeAgentBackend starts a fake handle whose stream echoes the requested
// model id back in the text, so a chat response proves which adapter served
// the request.
type threeAgentBackend struct {
	mu      sync.Mutex
	handles map[string]*fakeHandle
	started []runtime.StartRequest
}

func (b *threeAgentBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.ExecutionHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := newFakeHandle(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "reply-from-" + req.ModelID},
			runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10}},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})
	b.handles[req.SessionID] = h
	b.started = append(b.started, req)
	return h, nil
}

func (b *threeAgentBackend) Preflight(ctx context.Context) error { return nil }

func (b *threeAgentBackend) StartRequests() []runtime.StartRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]runtime.StartRequest, len(b.started))
	copy(out, b.started)
	return out
}

// codexDesc / claudeDesc / kimiDesc are the fake descriptors for the three
// first-generation agents, mirroring their real lifecycle declarations.
var claudeDesc = runtime.Descriptor{
	ModelID:       "claude-code",
	DisplayName:   "Claude Code",
	Description:   "fake claude-code for api tests",
	LifecycleMode: runtime.LifecyclePersistentProcess,
	Capabilities: runtime.Capabilities{
		Streaming: true, ToolCalls: true, Reasoning: true,
		Permission: true, Resume: true, MultiTurn: true,
	},
}

var kimiDesc = runtime.Descriptor{
	ModelID:       "kimi",
	DisplayName:   "Kimi",
	Description:   "fake kimi for api tests",
	LifecycleMode: runtime.LifecyclePersistentProcess,
	Capabilities: runtime.Capabilities{
		Streaming: true, ToolCalls: true, Reasoning: true,
		Resume: true, MultiTurn: true,
	},
}

// newThreeAgentServer builds the OpenAI handler with the three fake adapters
// registered by their public model ids.
func newThreeAgentServer(t *testing.T) (*httptest.Server, *threeAgentBackend) {
	t.Helper()
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	mustRegister(t, reg, "claude-code", claudeDesc, nil)
	mustRegister(t, reg, "kimi", kimiDesc, nil)

	backend := &threeAgentBackend{handles: make(map[string]*fakeHandle)}
	h := NewHandler(Options{
		Registry:     reg,
		Store:        runtime.NewMemorySessionStore(),
		Backend:      backend,
		CallerTokens: map[string]string{testToken: testOwner},
		TurnTimeout:  5 * time.Second,
		UsageGrace:   15 * time.Millisecond,
		Enabled:      func(name string) bool { return true },
	})
	t.Cleanup(h.Close)
	ts := httptest.NewServer(h.Routes())
	t.Cleanup(ts.Close)
	return ts, backend
}

// TestThreeAgentDiscovery verifies /v1/models advertises exactly the three
// first-generation agents, sorted, regardless of their lifecycle mode.
func TestThreeAgentDiscovery(t *testing.T) {
	ts, _ := newThreeAgentServer(t)

	resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", testToken, testOwner, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	var ml struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(readBody(t, resp), &ml); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if ml.Object != "list" {
		t.Errorf("object = %q, want list", ml.Object)
	}
	if len(ml.Data) != 3 {
		t.Fatalf("data length = %d, want 3 (codex, claude-code, kimi): %+v", len(ml.Data), ml.Data)
	}
	want := []string{"claude-code", "codex", "kimi"} // sorted by id
	for i, w := range want {
		if ml.Data[i].ID != w {
			t.Fatalf("data[%d].id = %q, want %q (sorted)", i, ml.Data[i].ID, w)
		}
	}
}

// TestThreeAgentRouting verifies the same /v1/chat/completions endpoint routes
// by `model` to three distinct adapters: each request's start record carries
// the requested model, and the user-visible reply echoes the serving adapter.
func TestThreeAgentRouting(t *testing.T) {
	ts, backend := newThreeAgentServer(t)

	for _, model := range []string{"codex", "claude-code", "kimi"} {
		resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq(model, false, defaultMessages()))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", model, resp.StatusCode, readBody(t, resp))
		}
		var cb completionBody
		if err := json.Unmarshal(readBody(t, resp), &cb); err != nil {
			t.Fatalf("%s: decode completion: %v", model, err)
		}
		if cb.Model != model {
			t.Errorf("%s: response model = %q, want %q", model, cb.Model, model)
		}
		if len(cb.Choices) != 1 {
			t.Fatalf("%s: choices len = %d", model, len(cb.Choices))
		}
		ch := cb.Choices[0]
		if ch.Message.Content == nil || *ch.Message.Content != "reply-from-"+model {
			t.Errorf("%s: content = %v, want reply-from-%s (proves the right adapter served)", model, ch.Message.Content, model)
		}
		if ch.FinishReason == nil || *ch.FinishReason != "stop" {
			t.Errorf("%s: finish_reason = %v, want stop", model, ch.FinishReason)
		}
	}

	started := backend.StartRequests()
	if len(started) != 3 {
		t.Fatalf("start requests = %d, want 3", len(started))
	}
	seen := map[string]bool{}
	for _, sr := range started {
		if sr.ModelID != "codex" && sr.ModelID != "claude-code" && sr.ModelID != "kimi" {
			t.Errorf("start request model = %q, want one of the three agents", sr.ModelID)
		}
		seen[sr.ModelID] = true
	}
	for _, m := range []string{"codex", "claude-code", "kimi"} {
		if !seen[m] {
			t.Errorf("no execution started for model %q", m)
		}
	}
}

// TestThreeAgentResume verifies each model's gateway session resumes onto the
// same adapter: a second turn with the X-Gateway-Session-Id reuses the
// execution (no new start) and still echoes the same adapter.
func TestThreeAgentResume(t *testing.T) {
	ts, backend := newThreeAgentServer(t)

	for _, model := range []string{"codex", "claude-code", "kimi"} {
		resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq(model, false, defaultMessages()))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s turn1: status = %d", model, resp.StatusCode)
		}
		sid := resp.Header.Get("X-Gateway-Session-Id")
		if sid == "" {
			t.Fatalf("%s: no X-Gateway-Session-Id in turn 1", model)
		}

		resp2 := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq(model, false, defaultMessages()),
			map[string]string{"X-Gateway-Session-Id": sid})
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("%s turn2: status = %d", model, resp2.StatusCode)
		}
		var cb completionBody
		if err := json.Unmarshal(readBody(t, resp2), &cb); err != nil {
			t.Fatalf("%s: decode completion: %v", model, err)
		}
		if cb.Choices[0].Message.Content == nil || *cb.Choices[0].Message.Content != "reply-from-"+model {
			t.Errorf("%s resume: content = %v, want reply-from-%s", model, cb.Choices[0].Message.Content, model)
		}
	}

	// Three sessions total; each had exactly one execution start (resumed, not
	// restarted).
	started := backend.StartRequests()
	if len(started) != 3 {
		t.Fatalf("start requests = %d, want 3 (each model started once and resumed)", len(started))
	}
}

// TestThreeAgentStreaming verifies streaming responses route per model too:
// the SSE chunks echo the serving adapter.
func TestThreeAgentStreaming(t *testing.T) {
	ts, _ := newThreeAgentServer(t)

	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("claude-code", true, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	events := splitSSE(t, readBody(t, resp))
	if len(events) < 3 {
		t.Fatalf("sse events = %d, want >= 3", len(events))
	}
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("last sse event = %q, want [DONE]", events[len(events)-1])
	}
	var content strings.Builder
	for i := 0; i < len(events)-1; i++ {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(events[i]), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
		}
	}
	if got := content.String(); got != "reply-from-claude-code" {
		t.Errorf("streamed content = %q, want reply-from-claude-code", got)
	}
}
