//go:build agent_ref

package codex

import (
	"reflect"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

func TestConfiguredModels_BoundaryConditions(t *testing.T) {
	a := &Agent{
		providers: []core.ProviderConfig{
			{Models: []core.ModelOption{{Name: "first"}}},
			{Models: []core.ModelOption{{Name: "second"}}},
		},
	}

	tests := []struct {
		name      string
		activeIdx int
		wantNil   bool
		wantName  string
	}{
		{name: "negative index", activeIdx: -1, wantNil: true},
		{name: "out of range", activeIdx: 2, wantNil: true},
		{name: "valid index", activeIdx: 1, wantName: "second"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a.activeIdx = tt.activeIdx
			got := a.configuredModels()
			if tt.wantNil {
				if got != nil {
					t.Fatalf("configuredModels() = %v, want nil", got)
				}
				return
			}
			if len(got) != 1 || got[0].Name != tt.wantName {
				t.Fatalf("configuredModels() = %v, want %q", got, tt.wantName)
			}
		})
	}
}

func TestGetModel_PrefersActiveProviderModel(t *testing.T) {
	a := &Agent{
		model: "gpt-4.1-mini",
		providers: []core.ProviderConfig{
			{Name: "openai", Model: "gpt-5.4"},
		},
		activeIdx: 0,
	}

	if got := a.GetModel(); got != "gpt-5.4" {
		t.Fatalf("GetModel() = %q, want gpt-5.4", got)
	}
}

func TestNormalizeAppServerURL_StdIOIsExplicit(t *testing.T) {
	for _, raw := range []string{"stdio", " stdio "} {
		if got := normalizeAppServerURL(raw); got != "stdio://" {
			t.Fatalf("normalizeAppServerURL(%q) = %q, want stdio://", raw, got)
		}
	}
}

func TestNormalizeAppServerURL_EmptyKeepsWebSocketDefault(t *testing.T) {
	if got := normalizeAppServerURL(""); got != "ws://127.0.0.1:3845" {
		t.Fatalf("normalizeAppServerURL(empty) = %q, want ws://127.0.0.1:3845", got)
	}
}

func TestWorkspaceAgentOptions_PreservesStdIOAppServerURL(t *testing.T) {
	a := &Agent{
		backend:      "app_server",
		appServerURL: normalizeAppServerURL("stdio"),
	}

	opts := a.WorkspaceAgentOptions()
	if got := opts["app_server_url"]; got != "stdio://" {
		t.Fatalf("WorkspaceAgentOptions()[app_server_url] = %#v, want stdio://", got)
	}
}

// modelFromLiveConfig surfaces the single model configured in Codex's live config
// (auth.json + config.toml) as a model option. Used as the AvailableModels
// fallback so config/providers returns the user's real model instead of the
// hardcoded OpenAI default list.
func TestModelFromLiveConfig(t *testing.T) {
	got := modelFromLiveConfig(core.LiveProviderConfig{Model: "gpt-5.4"})
	want := []core.ModelOption{{Name: "gpt-5.4", Desc: "gpt-5.4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelFromLiveConfig() = %+v, want %+v", got, want)
	}
}

func TestModelFromLiveConfig_EmptyReturnsNil(t *testing.T) {
	if got := modelFromLiveConfig(core.LiveProviderConfig{}); got != nil {
		t.Fatalf("modelFromLiveConfig(empty) = %+v, want nil", got)
	}
}
