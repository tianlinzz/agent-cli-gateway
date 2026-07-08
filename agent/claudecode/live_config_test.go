package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// withTempHome runs fn with HOME (and therefore ~/.claude) pointed at a fresh
// temp dir. The original HOME is restored afterwards.
// withTempHome runs fn with HOME (Unix) and USERPROFILE (Windows) both pointed
// at a fresh temp dir, so os.UserHomeDir() resolves under it on every platform.
// The original values are restored afterwards. This matters because the tests
// must NOT read or write the developer's real ~/.claude/settings.json.
func withTempHome(t *testing.T, fn func(homeDir string)) {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(setHomeEnvForTest(t, dir))
	fn(dir)
}

// setHomeEnvForTest points both HOME and USERPROFILE at dir and returns a
// cleanup that restores the prior values. Setting both is required because
// os.UserHomeDir() reads USERPROFILE on Windows but HOME on Unix.
func setHomeEnvForTest(t *testing.T, dir string) func() {
	t.Helper()
	var cleanups []func()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		prev, had := os.LookupEnv(key)
		os.Setenv(key, dir)
		k, p, h := key, prev, had
		cleanups = append(cleanups, func() {
			if h {
				os.Setenv(k, p)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	return func() {
		for _, c := range cleanups {
			c()
		}
	}
}

// writeSettings writes a JSON object to ~/.claude/settings.json inside homeDir.
func writeSettings(t *testing.T, homeDir string, v map[string]any) {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readSettingsAsMap reads ~/.claude/settings.json back into a generic map.
func readSettingsAsMap(t *testing.T, homeDir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(homeDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClaude_ReadLiveProvider_EmptyWhenNoFile(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		cfg, err := a.ReadLiveProvider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIKey != "" || cfg.BaseURL != "" || cfg.Model != "" {
			t.Errorf("expected zero value, got %+v", cfg)
		}
	})
}

func TestClaude_ReadLiveProvider_PrefersAuthTokenOverAPIKey(t *testing.T) {
	// When both ANTHROPIC_AUTH_TOKEN and ANTHROPIC_API_KEY are present, AUTH_TOKEN
	// wins — this mirrors how the runtime treats third-party base URLs.
	withTempHome(t, func(homeDir string) {
		writeSettings(t, homeDir, map[string]any{
			"env": map[string]any{
				"ANTHROPIC_AUTH_TOKEN": "sk-bearer",
				"ANTHROPIC_API_KEY":    "sk-plain",
				"ANTHROPIC_BASE_URL":   "https://relay.example.com",
				"ANTHROPIC_MODEL":      "claude-sonnet-4",
			},
		})
		a := &Agent{}
		cfg, err := a.ReadLiveProvider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIKey != "sk-bearer" {
			t.Errorf("apiKey = %q, want sk-bearer", cfg.APIKey)
		}
		if cfg.BaseURL != "https://relay.example.com" {
			t.Errorf("baseUrl = %q", cfg.BaseURL)
		}
		if cfg.Model != "claude-sonnet-4" {
			t.Errorf("model = %q", cfg.Model)
		}
	})
}

func TestClaude_ReadLiveProvider_SurfacesPassthroughEnv(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		writeSettings(t, homeDir, map[string]any{
			"env": map[string]any{
				"CLAUDE_CODE_USE_BEDROCK": "1",
				"ANTHROPIC_MODEL":         "claude-sonnet-4",
			},
		})
		a := &Agent{}
		cfg, _ := a.ReadLiveProvider(context.Background())
		if cfg.Env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
			t.Errorf("passthrough env missing bedrock flag: %+v", cfg.Env)
		}
		// Managed keys must NOT leak into Env.
		if _, leaked := cfg.Env["ANTHROPIC_MODEL"]; leaked {
			t.Error("ANTHROPIC_MODEL leaked into Env")
		}
	})
}

func TestClaude_WriteLiveProvider_ThirdParty_UsesAuthTokenAndClearsAPIKey(t *testing.T) {
	// Writing a provider with baseURL must use ANTHROPIC_AUTH_TOKEN (Bearer)
	// and explicitly clear ANTHROPIC_API_KEY — otherwise Claude Code tries to
	// validate the plain key against api.anthropic.com and hangs.
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		err := a.WriteLiveProvider(context.Background(), core.LiveProviderConfig{
			APIKey:  "sk-relay",
			BaseURL: "https://relay.example.com",
			Model:   "claude-sonnet-4",
		})
		if err != nil {
			t.Fatal(err)
		}
		settings := readSettingsAsMap(t, homeDir)
		env, _ := settings["env"].(map[string]any)
		if env["ANTHROPIC_AUTH_TOKEN"] != "sk-relay" {
			t.Errorf("AUTH_TOKEN = %v, want sk-relay", env["ANTHROPIC_AUTH_TOKEN"])
		}
		if env["ANTHROPIC_BASE_URL"] != "https://relay.example.com" {
			t.Errorf("BASE_URL = %v", env["ANTHROPIC_BASE_URL"])
		}
		if env["ANTHROPIC_MODEL"] != "claude-sonnet-4" {
			t.Errorf("MODEL = %v", env["ANTHROPIC_MODEL"])
		}
		if _, stillThere := env["ANTHROPIC_API_KEY"]; stillThere {
			t.Error("ANTHROPIC_API_KEY must be cleared for third-party base URL")
		}
	})
}

func TestClaude_WriteLiveProvider_FirstParty_UsesAPIKey(t *testing.T) {
	// Without a baseURL the provider is treated as first-party (api.anthropic.com),
	// so ANTHROPIC_API_KEY is used and AUTH_TOKEN is cleared.
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		err := a.WriteLiveProvider(context.Background(), core.LiveProviderConfig{
			APIKey: "sk-official",
		})
		if err != nil {
			t.Fatal(err)
		}
		env, _ := readSettingsAsMap(t, homeDir)["env"].(map[string]any)
		if env["ANTHROPIC_API_KEY"] != "sk-official" {
			t.Errorf("API_KEY = %v", env["ANTHROPIC_API_KEY"])
		}
		if _, leaked := env["ANTHROPIC_AUTH_TOKEN"]; leaked {
			t.Error("AUTH_TOKEN must be cleared for first-party")
		}
		if _, leaked := env["ANTHROPIC_BASE_URL"]; leaked {
			t.Error("BASE_URL must be cleared when not set")
		}
	})
}

func TestClaude_WriteLiveProvider_PreservesOtherFields(t *testing.T) {
	// settings.json typically holds permissions, hooks, etc. The management API
	// must NOT clobber them when writing the provider env.
	withTempHome(t, func(homeDir string) {
		writeSettings(t, homeDir, map[string]any{
			"permissions": map[string]any{
				"allow": []string{"Bash(go:test)"},
			},
			"hooks": map[string]any{
				"PreToolUse": "kept",
			},
			"env": map[string]any{
				"EXISTING_VAR":    "kept",
				"ANTHROPIC_MODEL": "old-model",
			},
		})

		a := &Agent{}
		err := a.WriteLiveProvider(context.Background(), core.LiveProviderConfig{
			APIKey: "sk-new",
			Model:  "new-model",
		})
		if err != nil {
			t.Fatal(err)
		}

		settings := readSettingsAsMap(t, homeDir)
		// Other top-level fields preserved.
		perms, _ := settings["permissions"].(map[string]any)
		if perms["allow"] == nil {
			t.Error("permissions.allow was clobbered")
		}
		if settings["hooks"] == nil {
			t.Error("hooks were clobbered")
		}
		// Unrelated env preserved; managed env updated.
		env, _ := settings["env"].(map[string]any)
		if env["EXISTING_VAR"] != "kept" {
			t.Error("EXISTING_VAR was clobbered")
		}
		if env["ANTHROPIC_MODEL"] != "new-model" {
			t.Errorf("ANTHROPIC_MODEL = %v, want new-model", env["ANTHROPIC_MODEL"])
		}
	})
}

func TestClaude_WriteLiveProvider_RoundTrip(t *testing.T) {
	// Write then read back — should match.
	withTempHome(t, func(_ string) {
		a := &Agent{}
		original := core.LiveProviderConfig{
			APIKey:  "sk-rt",
			BaseURL: "https://relay.example.com",
			Model:   "claude-opus-4",
			Env:     map[string]string{"FOO": "bar"},
		}
		if err := a.WriteLiveProvider(context.Background(), original); err != nil {
			t.Fatal(err)
		}
		got, err := a.ReadLiveProvider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.APIKey != original.APIKey || got.BaseURL != original.BaseURL || got.Model != original.Model {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got, original)
		}
		if got.Env["FOO"] != "bar" {
			t.Errorf("passthrough env lost in round-trip: %+v", got.Env)
		}
	})
}

func TestClaude_marshalSorted_HasStableKeyOrder(t *testing.T) {
	// Inserting keys in different orders must yield identical bytes.
	a := map[string]any{"z": "1", "a": "2", "m": map[string]any{"y": "1", "b": "2"}}
	b := map[string]any{"a": "2", "m": map[string]any{"b": "2", "y": "1"}, "z": "1"}
	outA, _ := marshalSorted(a)
	outB, _ := marshalSorted(b)
	if string(outA) != string(outB) {
		t.Errorf("marshalSorted not stable:\n--- A ---\n%s\n--- B ---\n%s", outA, outB)
	}
}

// -----------------------------------------------------------------------------
// LiveConfigFileProvider (raw whole-file editing)
// -----------------------------------------------------------------------------

func TestClaude_ListLiveConfigFiles_IncludesSettings(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		writeSettings(t, homeDir, map[string]any{
			"env": map[string]any{"ANTHROPIC_API_KEY": "sk-test"},
		})
		a := &Agent{}
		files, err := a.ListLiveConfigFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].Name != "settings.json" {
			t.Fatalf("files = %+v", files)
		}
		if !strings.Contains(files[0].Content, "sk-test") {
			t.Errorf("settings.json content = %q", files[0].Content)
		}
	})
}

func TestClaude_ListLiveConfigFiles_MissingFileIsEmptyNotError(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		files, err := a.ListLiveConfigFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatalf("files len = %d, want 1", len(files))
		}
		if files[0].Content != "" {
			t.Errorf("missing settings.json should have empty content, got %q", files[0].Content)
		}
	})
}

func TestClaude_WriteLiveConfigFile_RoundTrip(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		// Full settings with permissions + hooks + env — fields the provider
		// triple endpoint can't manage.
		content := `{
  "permissions": {"allow": ["Bash(git:*)"]},
  "env": {
    "ANTHROPIC_BASE_URL": "https://relay.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-bearer",
    "ANTHROPIC_MODEL": "claude-sonnet-4"
  }
}`
		if err := a.WriteLiveConfigFile(context.Background(), "settings.json", []byte(content)); err != nil {
			t.Fatal(err)
		}
		// Written verbatim — key order and formatting preserved.
		got, err := os.ReadFile(filepath.Join(homeDir, ".claude", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("settings.json not written verbatim.\nwant:\n%s\ngot:\n%s", content, string(got))
		}
	})
}

func TestClaude_WriteLiveConfigFile_InvalidJSONRejected(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		err := a.WriteLiveConfigFile(context.Background(), "settings.json", []byte("{not json"))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
		// Disk untouched.
		if _, err := os.Stat(filepath.Join(homeDir, ".claude", "settings.json")); !os.IsNotExist(err) {
			t.Errorf("broken JSON was persisted to disk")
		}
	})
}

func TestClaude_WriteLiveConfigFile_UnknownFileRejected(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		err := a.WriteLiveConfigFile(context.Background(), "claude.json", []byte("{}"))
		if err == nil {
			t.Fatal("expected error for unknown file name")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should contain 'not found', got: %v", err)
		}
	})
}

func TestClaude_WriteLiveConfigFile_EmptyContentAllowed(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		// Empty content clears the file (allowed — not all empties are invalid).
		if err := a.WriteLiveConfigFile(context.Background(), "settings.json", []byte("")); err != nil {
			t.Fatalf("empty content should be allowed: %v", err)
		}
	})
}
