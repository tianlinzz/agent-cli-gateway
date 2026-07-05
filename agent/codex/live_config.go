package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// Live config files for Codex, both under $CODEX_HOME (default ~/.codex):
//   - auth.json   holds OPENAI_API_KEY at the top level (plus OAuth material
//                 the user may have from a ChatGPT login — must be preserved).
//   - config.toml holds the active model / model_provider at the top level and
//                 base_url inside [model_providers.<active>].
//
// The two files must change together: writing auth.json but failing on
// config.toml would leave the user half-switched. writeCodexLiveAtomic
// implements the cc-switch rollback pattern (back up old auth.json bytes,
// write auth.json, write config.toml, restore auth.json on failure).

// activeCodexProviderID reads config.toml's top-level model_provider.
// Returns "" when unset.
func activeCodexProviderID(configText string) string {
	var doc struct {
		ModelProvider string `toml:"model_provider"`
	}
	if err := toml.Unmarshal([]byte(configText), &doc); err != nil {
		return ""
	}
	return doc.ModelProvider
}

// topLevelCodexModel reads config.toml's top-level model.
func topLevelCodexModel(configText string) string {
	var doc struct {
		Model string `toml:"model"`
	}
	if err := toml.Unmarshal([]byte(configText), &doc); err != nil {
		return ""
	}
	return doc.Model
}

// codexProviderBaseURL reads base_url from [model_providers.<id>] when <id> is
// the active model_provider; falls back to the top-level base_url otherwise.
// (Matches cc-switch's extract_codex_base_url: never read a non-active
// provider's base_url, to avoid leaking unrelated credentials.)
func codexProviderBaseURL(configText, activeID string) string {
	if activeID != "" {
		var doc struct {
			ModelProviders map[string]struct {
				BaseURL string `toml:"base_url"`
			} `toml:"model_providers"`
		}
		if toml.Unmarshal([]byte(configText), &doc) == nil {
			if p, ok := doc.ModelProviders[activeID]; ok && p.BaseURL != "" {
				return p.BaseURL
			}
		}
	}
	var top struct {
		BaseURL string `toml:"base_url"`
	}
	if toml.Unmarshal([]byte(configText), &top) == nil {
		return top.BaseURL
	}
	return ""
}

// authJSONAPIKey extracts the top-level OPENAI_API_KEY from auth.json bytes.
// Returns "" when missing or unparseable.
func authJSONAPIKey(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	if v, ok := doc["OPENAI_API_KEY"].(string); ok {
		return v
	}
	return ""
}

// setAuthAPIKey returns a copy of raw auth.json bytes with OPENAI_API_KEY set
// to apiKey (or removed when apiKey is empty). All other top-level keys —
// crucially any OAuth login material (tokens.* / auth_mode) — are preserved.
func setAuthAPIKey(raw []byte, apiKey string) ([]byte, error) {
	doc := make(map[string]any)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("codex: parse auth.json: %w", err)
		}
	}
	if apiKey == "" {
		delete(doc, "OPENAI_API_KEY")
	} else {
		doc["OPENAI_API_KEY"] = apiKey
	}
	// auth_mode is set to "apikey" when a key is present, matching the
	// existing ensureCodexAuth convention; cleared otherwise.
	if apiKey != "" {
		doc["auth_mode"] = "apikey"
	} else {
		delete(doc, "auth_mode")
	}
	return json.MarshalIndent(doc, "", "  ")
}

// setConfigProvider rewrites config.toml text so that model / model_provider
// at the top level and base_url under [model_providers.<id>] reflect the given
// values. Uses BurntSushi decode → generic map → re-encode, which loses
// comments but is stable and dependency-light. Unknown sections are preserved.
//
// providerID defaults to "custom" when baseURL is set but no explicit id is
// configured; this matches cc-switch's unified-session bucket convention
// without dragging in its full catalog/bucket machinery.
func setConfigProvider(configText string, model, providerID, baseURL string) (string, error) {
	root := make(map[string]any)
	if configText != "" {
		if err := toml.Unmarshal([]byte(configText), &root); err != nil {
			return "", fmt.Errorf("codex: parse config.toml: %w", err)
		}
	}

	// Top-level model / model_provider.
	if model != "" {
		root["model"] = model
	} else {
		delete(root, "model")
	}

	activeID := providerID
	if baseURL != "" && activeID == "" {
		activeID = "custom"
	}
	if baseURL != "" {
		root["model_provider"] = activeID
		// Inject / overwrite [model_providers.<activeID>] with at least base_url.
		providers, _ := root["model_providers"].(map[string]any)
		if providers == nil {
			providers = make(map[string]any)
		}
		entry, _ := providers[activeID].(map[string]any)
		if entry == nil {
			entry = make(map[string]any)
		}
		entry["base_url"] = baseURL
		// Keep name/env_key aligned with the existing buildProviderSection output
		// so a later ensureCodexProviderConfig call is a no-op.
		if _, ok := entry["name"]; !ok {
			entry["name"] = activeID
		}
		if _, ok := entry["env_key"]; !ok {
			entry["env_key"] = "OPENAI_API_KEY"
		}
		providers[activeID] = entry
		root["model_providers"] = providers
	} else {
		// No baseURL: leave model_provider alone (do not force-clear — the user
		// may have an official OpenAI/Bedrock provider configured that needs
		// the field). Only model is governed by the basic triple here.
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(root); err != nil {
		return "", fmt.Errorf("codex: encode config.toml: %w", err)
	}
	return buf.String(), nil
}

// ReadLiveProvider reads the active provider from Codex's auth.json +
// config.toml. Implements core.LiveConfigProvider.
func (a *Agent) ReadLiveProvider(_ context.Context) (core.LiveProviderConfig, error) {
	home, err := resolveCodexHomeForConfig(a.codexHome)
	if err != nil {
		return core.LiveProviderConfig{}, fmt.Errorf("codex: resolve codex home: %w", err)
	}
	authRaw, _ := os.ReadFile(filepath.Join(home, "auth.json"))
	configRaw, _ := os.ReadFile(filepath.Join(home, "config.toml"))

	activeID := activeCodexProviderID(string(configRaw))
	return core.LiveProviderConfig{
		APIKey:  authJSONAPIKey(authRaw),
		BaseURL: codexProviderBaseURL(string(configRaw), activeID),
		Model:   topLevelCodexModel(string(configRaw)),
	}, nil
}

// WriteLiveProvider writes the provider into Codex's auth.json + config.toml
// atomically: if the second write (config.toml) fails, auth.json is restored
// to its previous bytes. Implements core.LiveConfigProvider.
//
// This is the cc-switch write_codex_live_atomic pattern, ported to Go:
//  1. Read & back up old auth.json bytes (for rollback; nil if absent).
//  2. Build & validate the new config.toml text in-memory BEFORE touching the
//     filesystem, so a TOML encode error never leaves a half-written state.
//  3. Write auth.json (only if it actually changed).
//  4. Write config.toml. On failure, roll auth.json back to step 1's bytes.
func (a *Agent) WriteLiveProvider(_ context.Context, cfg core.LiveProviderConfig) error {
	home, err := resolveCodexHomeForConfig(a.codexHome)
	if err != nil {
		return fmt.Errorf("codex: resolve codex home: %w", err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("codex: mkdir codex home: %w", err)
	}

	authPath := filepath.Join(home, "auth.json")
	configPath := filepath.Join(home, "config.toml")

	// 1. Back up the current auth.json bytes (nil if absent).
	oldAuth, _ := os.ReadFile(authPath)

	// 2. Build & validate new config.toml text in-memory first.
	oldConfig, _ := os.ReadFile(configPath)
	newConfigText, err := setConfigProvider(string(oldConfig), cfg.Model, "", cfg.BaseURL)
	if err != nil {
		return err
	}
	// Pre-validate the marshalled TOML so we never write a syntactically broken
	// config — matches cc-switch's toml::from_str check before writing.
	var probe map[string]any
	if err := toml.Unmarshal([]byte(newConfigText), &probe); err != nil {
		return fmt.Errorf("codex: rejected config.toml (would not parse): %w", err)
	}

	// 3. Build the new auth.json bytes (only if it actually changes).
	newAuth, err := setAuthAPIKey(oldAuth, cfg.APIKey)
	if err != nil {
		return err
	}
	authChanged := !bytes.Equal(oldAuth, newAuth)
	if authChanged {
		if err := core.AtomicWriteFile(authPath, append(newAuth, '\n'), 0o600); err != nil {
			return fmt.Errorf("codex: write auth.json: %w", err)
		}
	}

	// 4. Write config.toml. On failure, roll auth.json back to its prior bytes.
	if err := core.AtomicWriteFile(configPath, []byte(newConfigText), 0o644); err != nil {
		if authChanged {
			if oldAuth != nil {
				if rerr := core.AtomicWriteFile(authPath, oldAuth, 0o600); rerr != nil {
					return fmt.Errorf("codex: write config.toml: %w (auth.json rollback also failed: %v)", err, rerr)
				}
			} else {
				_ = os.Remove(authPath)
			}
		}
		return fmt.Errorf("codex: write config.toml: %w", err)
	}
	return nil
}
