package worker

import (
	"reflect"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestAgentExecutionConfigRoundTrip(t *testing.T) {
	want := StartSessionReq{ModelID: "codex", SessionID: "s", CallerID: "c", WorkspaceID: "w",
		AgentConfig: runtime.AgentExecutionConfig{Command: "codex-test", DefaultModel: "o4-mini",
			Permission: "deny", TurnTimeout: 17 * time.Second, MaxConcurrency: 3,
			Env: map[string]string{"CODEX_API_KEY": "redacted"}, InjectSystemPrompt: true}}
	p, err := toProtoStartRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fromProtoStartRequest(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AgentConfig, want.AgentConfig) {
		t.Fatalf("agent config round trip mismatch: got %#v want %#v", got.AgentConfig, want.AgentConfig)
	}
}
