//go:build agent_ref

package claudecode

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
		model: "sonnet",
		providers: []core.ProviderConfig{
			{Name: "anthropic", Model: "opus"},
		},
		activeIdx: 0,
	}

	if got := a.GetModel(); got != "opus" {
		t.Fatalf("GetModel() = %q, want opus", got)
	}
}

// aliasModelsFromEnv parses claude code's ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU,FABLE}_MODEL
// env vars into model options. The alias name (opus/sonnet/haiku/fable) is what
// claude code accepts as --model; the CLI maps it to the real model via these env.
func TestAliasModelsFromEnv(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL":        "glm-5.2[1M]",
		"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME":   "Opus",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":      "claude-opus-4-6[1M]",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "Sonnet",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":       "claude-sonnet-5",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME":  "Haiku",
		"ANTHROPIC_DEFAULT_FABLE_MODEL":       "glm-5.2[1M]",
		// FABLE omits _NAME → falls back to "Fable".
	}
	got := aliasModelsFromEnv(env)
	want := []core.ModelOption{
		{Name: "opus", Desc: "Opus"},
		{Name: "sonnet", Desc: "Sonnet"},
		{Name: "haiku", Desc: "Haiku"},
		{Name: "fable", Desc: "Fable"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliasModelsFromEnv() = %+v, want %+v", got, want)
	}
}

func TestAliasModelsFromEnv_EmptyReturnsNil(t *testing.T) {
	if got := aliasModelsFromEnv(map[string]string{}); got != nil {
		t.Fatalf("aliasModelsFromEnv(empty) = %+v, want nil", got)
	}
}
