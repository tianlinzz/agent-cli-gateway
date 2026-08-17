package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	codexadapter "github.com/tianlinzz/agent-cli-gateway/adapters/codex"
	"github.com/tianlinzz/agent-cli-gateway/api/openai"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
)

// buildIntegrationBinary builds one in-repo main package (e.g. the REAL
// gateway-worker, or the hermetic fake codex CLI) into a short-lived temp dir.
func buildIntegrationBinary(t *testing.T, pkg string) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve integration test dir")
	}
	root := filepath.Dir(filepath.Dir(file))
	dir, err := os.MkdirTemp("", "gwint-bin-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", bin, "./"+pkg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// TestIntegration_RealWorkerDeadline_OF09b drives the REAL gateway-worker
// binary — the real codex adapter with the adapters/internal/bridge deadline
// watcher — against a hermetic fake codex app-server, through the real HTTP
// API and supervisor. It covers the code-review Phase 3 re-entry requirement:
// one deadline through the full path proves the worker-boundary timer and the
// API safety-net timer fire coherently (no double-cancel misjudgment), the
// surface is the unified turn_timeout error, and the persistent session stays
// reusable for the next turn.
func TestIntegration_RealWorkerDeadline_OF09b(t *testing.T) {
	fakeCLI := buildIntegrationBinary(t, "integration/fakecodexcli")
	realWorker := buildIntegrationBinary(t, "cmd/gateway-worker")

	deadline := 1500 * time.Millisecond
	h := newHarnessExt(t,
		func(cfg *worker.Config) {
			// The supervisor spawns the REAL gateway-worker; the worker-side
			// deadline for codex comes from the typed agent config (O-F08 →
			// O-F09b).
			cfg.WorkerExec = realWorker
			cfg.Agents = map[string]runtime.AgentExecutionConfig{
				"codex": {Command: []string{fakeCLI}, Permission: "auto", TurnTimeout: deadline},
			}
		},
		func(reg *runtime.Registry) {
			// Register the REAL codex adapter in the API process (Describe).
			if err := reg.Register("codex", codexadapter.New); err != nil {
				t.Fatalf("register real codex adapter: %v", err)
			}
		},
		func(o *openai.Options) {
			// The API layer fires the same effective deadline (the safety
			// net) as the worker boundary.
			o.TurnTimeout = deadline
		},
	)

	// Turn 1: a prompt the fake CLI never answers. Both deadline layers fire
	// at the same instant; the contract requires the unified timeout error
	// regardless of which layer wins.
	blockBody := map[string]any{
		"model":    "codex",
		"messages": []map[string]any{{"role": "user", "content": "block"}},
		"metadata": map[string]any{"workspace_id": "ws-deadline"},
	}
	resp := h.do("POST", "/v1/chat/completions", blockBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("blocked turn status = %d, want 504 (body %s)", resp.StatusCode, body)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "turn_timeout" {
		t.Fatalf("error code = %q, want turn_timeout", errBody.Error.Code)
	}
	sessionID := resp.Header.Get("X-Gateway-Session-Id")
	if sessionID == "" {
		t.Fatal("no session id on the timed-out response")
	}

	// Turn 2: the SAME session must still work — the worker's deadline abort
	// settled at the native turn boundary, the persistent process survived,
	// and no stale terminal is buffered ahead of the new turn.
	okBody := map[string]any{
		"model":    "codex",
		"messages": []map[string]any{{"role": "user", "content": "ok"}},
		"metadata": map[string]any{"workspace_id": "ws-deadline"},
	}
	resp2 := h.do("POST", "/v1/chat/completions", okBody, map[string]string{"X-Gateway-Session-Id": sessionID})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("follow-up turn status = %d, want 200 (session must stay usable; body %s)", resp2.StatusCode, body)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, "echo:ok") {
		t.Fatalf("follow-up completion = %#v, want the fake CLI's echo:ok answer", completion)
	}
}
