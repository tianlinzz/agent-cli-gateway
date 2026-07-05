package codex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// withTempCodexHome runs fn with a fresh temp dir and an Agent whose codexHome
// points at it. Ensures tests never touch the real ~/.codex.
func withTempCodexHome(t *testing.T, fn func(homeDir string, a *Agent)) {
	t.Helper()
	dir := t.TempDir()
	a := &Agent{codexHome: dir}
	fn(dir, a)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// -----------------------------------------------------------------------------
// pure helpers
// -----------------------------------------------------------------------------

func TestSetAuthAPIKey_PreservesOAuthMaterial(t *testing.T) {
	// auth.json often holds ChatGPT OAuth tokens alongside the API key.
	// setAuthAPIKey MUST preserve them when changing the key.
	original := []byte(`{
		"OPENAI_API_KEY": "old-key",
		"auth_mode": "apikey",
		"tokens": {"access_token": "tok-abc", "id_token": "id-xyz"},
		"last_refresh": "2025-01-01T00:00:00Z"
	}`)
	out, err := setAuthAPIKey(original, "new-key")
	if err != nil {
		t.Fatal(err)
	}
	// API key updated.
	if !bytes.Contains(out, []byte(`"new-key"`)) {
		t.Errorf("new key not written: %s", out)
	}
	// OAuth material preserved.
	if !bytes.Contains(out, []byte("tok-abc")) || !bytes.Contains(out, []byte("id-xyz")) {
		t.Errorf("OAuth tokens were dropped: %s", out)
	}
}

func TestSetAuthAPIKey_ClearingKeyRemovesIt(t *testing.T) {
	original := []byte(`{"OPENAI_API_KEY":"k","auth_mode":"apikey","tokens":{"access_token":"t"}}`)
	out, err := setAuthAPIKey(original, "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("OPENAI_API_KEY")) {
		t.Errorf("OPENAI_API_KEY should be removed: %s", out)
	}
	// OAuth token preserved even when key is cleared.
	if !bytes.Contains(out, []byte("access_token")) {
		t.Errorf("OAuth token lost when clearing key: %s", out)
	}
}

func TestSetConfigProvider_InjectsActiveProvider(t *testing.T) {
	// Starting from a config with unrelated sections, setting base_url should
	// add model_provider + [model_providers.custom] without dropping anything.
	original := `
# a comment that will be lost (BurntSushi round-trip, accepted per plan)
model = "gpt-4o"

[mcp_servers.filesystem]
command = "npx"
`
	out, err := setConfigProvider(original, "gpt-5", "", "https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Top-level model updated.
	if !contains(out, `model = "gpt-5"`) {
		t.Errorf("model not updated:\n%s", out)
	}
	// model_provider injected as "custom" (default id when baseURL set, id empty).
	if !contains(out, `model_provider = "custom"`) {
		t.Errorf("model_provider not injected:\n%s", out)
	}
	// base_url landed under [model_providers.custom].
	if !contains(out, `base_url = "https://relay.example.com"`) {
		t.Errorf("base_url missing:\n%s", out)
	}
	// MCP section preserved.
	if !contains(out, `[mcp_servers.filesystem]`) {
		t.Errorf("mcp_servers section dropped:\n%s", out)
	}
}

func TestSetConfigProvider_NoBaseURL_KeepsExistingProvider(t *testing.T) {
	// Without a baseURL the user may rely on an official provider (OpenAI/
	// Bedrock) — we must NOT clear model_provider in that case.
	original := `model = "x"
model_provider = "openai"
`
	out, err := setConfigProvider(original, "gpt-5", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, `model_provider = "openai"`) {
		t.Errorf("model_provider should be preserved when baseURL empty:\n%s", out)
	}
}

// -----------------------------------------------------------------------------
// ReadLiveProvider / WriteLiveProvider (happy path)
// -----------------------------------------------------------------------------

func TestCodex_ReadLiveProvider_EmptyWhenNoFiles(t *testing.T) {
	withTempCodexHome(t, func(_ string, a *Agent) {
		cfg, err := a.ReadLiveProvider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIKey != "" || cfg.BaseURL != "" || cfg.Model != "" {
			t.Errorf("expected zero value, got %+v", cfg)
		}
	})
}

func TestCodex_WriteThenRead_RoundTrip(t *testing.T) {
	withTempCodexHome(t, func(home string, a *Agent) {
		err := a.WriteLiveProvider(context.Background(), core.LiveProviderConfig{
			APIKey:  "sk-test",
			BaseURL: "https://relay.example.com",
			Model:   "gpt-5",
		})
		if err != nil {
			t.Fatal(err)
		}

		// Both files exist and contain the right fields.
		auth := readFileOrEmpty(t, filepath.Join(home, "auth.json"))
		if !contains(auth, `"OPENAI_API_KEY": "sk-test"`) {
			t.Errorf("auth.json missing key:\n%s", auth)
		}
		cfgText := readFileOrEmpty(t, filepath.Join(home, "config.toml"))
		if !contains(cfgText, `model = "gpt-5"`) {
			t.Errorf("config.toml missing model:\n%s", cfgText)
		}
		if !contains(cfgText, `base_url = "https://relay.example.com"`) {
			t.Errorf("config.toml missing base_url:\n%s", cfgText)
		}

		// Read back.
		got, err := a.ReadLiveProvider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.APIKey != "sk-test" || got.BaseURL != "https://relay.example.com" || got.Model != "gpt-5" {
			t.Errorf("round-trip mismatch: got %+v", got)
		}
	})
}

func TestCodex_WriteLiveProvider_PreservesOAuthInAuth(t *testing.T) {
	withTempCodexHome(t, func(home string, a *Agent) {
		// Pre-existing auth.json with ChatGPT OAuth tokens.
		writeFile(t, filepath.Join(home, "auth.json"), `{
			"OPENAI_API_KEY": "old",
			"tokens": {"access_token": "oauth-tok"}
		}`)

		err := a.WriteLiveProvider(context.Background(), core.LiveProviderConfig{
			APIKey:  "new-key",
			BaseURL: "https://relay.example.com",
			Model:   "gpt-5",
		})
		if err != nil {
			t.Fatal(err)
		}
		auth := readFileOrEmpty(t, filepath.Join(home, "auth.json"))
		if !contains(auth, "oauth-tok") {
			t.Errorf("OAuth token dropped from auth.json:\n%s", auth)
		}
		if !contains(auth, `"new-key"`) {
			t.Errorf("new API key not written:\n%s", auth)
		}
	})
}

// -----------------------------------------------------------------------------
// REGRESSION TEST: dual-file atomic write + rollback
// -----------------------------------------------------------------------------
// This is the cc-switch write_codex_live_atomic pattern. If config.toml write
// fails AFTER auth.json was written, auth.json MUST be rolled back to its
// pre-call bytes — otherwise the user is left half-switched (new key, old
// endpoint) which is the worst state.
//
// We force the config.toml write to fail by making its target path a directory
// (so AtomicWriteFile's rename target is obstructed). This reproduces the
// "second write failed" scenario without mocking.

func TestCodex_WriteLiveProvider_RollsBackAuthWhenConfigWriteFails(t *testing.T) {
	withTempCodexHome(t, func(home string, a *Agent) {
		authPath := filepath.Join(home, "auth.json")
		configPath := filepath.Join(home, "config.toml")

		// Seed a known auth.json we can check against after rollback.
		originalAuth := `{"OPENAI_API_KEY": "original","tokens":{"access_token":"keep-me"}}`
		writeFile(t, authPath, originalAuth)
		originalBytes, _ := os.ReadFile(authPath)

		// Obstruct config.toml's target path with a directory so the rename in
		// AtomicWriteFile cannot land. This reliably fails on both POSIX and
		// Windows.
		if err := os.MkdirAll(configPath, 0o755); err != nil {
			t.Fatal(err)
		}

		err := a.WriteLiveProvider(context.Background(), core.LiveProviderConfig{
			APIKey:  "switched-key",
			BaseURL: "https://relay.example.com",
			Model:   "gpt-5",
		})
		if err == nil {
			t.Fatal("expected WriteLiveProvider to fail because config.toml is obstructed")
		}

		// Regression assertion: auth.json MUST be byte-identical to its
		// pre-call state. If rollback is broken, "switched-key" leaks in.
		afterAuth, _ := os.ReadFile(authPath)
		if !bytes.Equal(originalBytes, afterAuth) {
			t.Fatalf("REGRESSION: auth.json not rolled back on config.toml failure.\nbefore: %s\nafter:  %s",
				originalBytes, afterAuth)
		}
		if bytes.Contains(afterAuth, []byte("switched-key")) {
			t.Fatal("REGRESSION: switched key leaked into auth.json — rollback failed")
		}
	})
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
