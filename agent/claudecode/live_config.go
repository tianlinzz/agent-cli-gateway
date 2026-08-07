//go:build agent_ref

package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// ~/.claude/settings.json field names that map onto LiveProviderConfig.
// Settings.json stores provider credentials inside its top-level "env" object.
const (
	settingsEnvAPIKey  = "ANTHROPIC_API_KEY"
	settingsEnvAuthKey = "ANTHROPIC_AUTH_TOKEN"
	settingsEnvBaseURL = "ANTHROPIC_BASE_URL"
	settingsEnvModel   = "ANTHROPIC_MODEL"
)

// claudeConfigDir returns the path to ~/.claude, creating it on demand so a
// fresh install (no settings.json yet) can still be written by the management
// API.
func claudeConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claudecode: cannot determine home dir: %w", err)
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("claudecode: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// claudeSettingsPath returns the path to ~/.claude/settings.json. The directory
// is created on demand so a fresh install (no settings.json yet) can still be
// written by the management API.
func claudeSettingsPath() (string, error) {
	dir, err := claudeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ReadLiveProvider reads the active provider from ~/.claude/settings.json.
// Implements core.LiveConfigProvider. The auth token (ANTHROPIC_AUTH_TOKEN,
// used for third-party base URLs) is preferred over ANTHROPIC_API_KEY when
// both are present, mirroring providerEnvLocked's runtime behaviour.
func (a *Agent) ReadLiveProvider(_ context.Context) (core.LiveProviderConfig, error) {
	path, err := claudeSettingsPath()
	if err != nil {
		return core.LiveProviderConfig{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return core.LiveProviderConfig{}, nil
		}
		return core.LiveProviderConfig{}, fmt.Errorf("claudecode: read settings.json: %w", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return core.LiveProviderConfig{}, fmt.Errorf("claudecode: parse settings.json: %w", err)
	}

	envMap, _ := settings["env"].(map[string]any)
	get := func(k string) string {
		if v, ok := envMap[k].(string); ok {
			return v
		}
		return ""
	}

	cfg := core.LiveProviderConfig{
		APIKey:  firstNonEmpty(get(settingsEnvAuthKey), get(settingsEnvAPIKey)),
		BaseURL: get(settingsEnvBaseURL),
		Model:   get(settingsEnvModel),
	}
	// Surface known passthrough env vars (e.g. CLAUDE_CODE_USE_BEDROCK). We only
	// copy keys outside the four managed ones so the round-trip is faithful.
	for k, v := range envMap {
		if k == settingsEnvAPIKey || k == settingsEnvAuthKey || k == settingsEnvBaseURL || k == settingsEnvModel {
			continue
		}
		if s, ok := v.(string); ok && s != "" {
			if cfg.Env == nil {
				cfg.Env = make(map[string]string)
			}
			cfg.Env[k] = s
		}
	}
	return cfg, nil
}

// WriteLiveProvider writes the provider into ~/.claude/settings.json's env
// object, preserving all other fields (top-level keys like "permissions",
// "hooks", other env entries). Implements core.LiveConfigProvider.
//
// Mapping (mirrors providerEnvLocked):
//   - BaseURL set   → ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN (Bearer); the
//                     plain ANTHROPIC_API_KEY is cleared because Claude Code
//                     validates it against api.anthropic.com and hangs for
//                     third-party endpoints.
//   - BaseURL empty → ANTHROPIC_API_KEY (x-api-key), AUTH_TOKEN cleared.
//   - Model         → ANTHROPIC_MODEL.
func (a *Agent) WriteLiveProvider(_ context.Context, cfg core.LiveProviderConfig) error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}

	// Read existing settings (or start fresh). Preserving other fields matters
	// because settings.json also holds permissions, hooks, model preferences.
	settings := make(map[string]any)
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("claudecode: parse settings.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("claudecode: read settings.json: %w", err)
	}

	envMap, _ := settings["env"].(map[string]any)
	if envMap == nil {
		envMap = make(map[string]any)
	}

	// Apply provider fields. Both auth keys are always (re)written so a switch
	// between third-party and first-party cleanly clears the previous one.
	delete(envMap, settingsEnvAuthKey)
	delete(envMap, settingsEnvAPIKey)
	if cfg.BaseURL != "" {
		envMap[settingsEnvBaseURL] = cfg.BaseURL
		if cfg.APIKey != "" {
			envMap[settingsEnvAuthKey] = cfg.APIKey
		}
	} else {
		delete(envMap, settingsEnvBaseURL)
		if cfg.APIKey != "" {
			envMap[settingsEnvAPIKey] = cfg.APIKey
		}
	}
	if cfg.Model != "" {
		envMap[settingsEnvModel] = cfg.Model
	} else {
		delete(envMap, settingsEnvModel)
	}
	// Apply passthrough env. Empty values clear the key.
	for k, v := range cfg.Env {
		if v == "" {
			delete(envMap, k)
			continue
		}
		envMap[k] = v
	}

	settings["env"] = envMap

	data, err := marshalSorted(settings)
	if err != nil {
		return fmt.Errorf("claudecode: marshal settings.json: %w", err)
	}
	if err := core.AtomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("claudecode: write settings.json: %w", err)
	}
	return nil
}

// marshalSorted encodes v as pretty JSON with object keys sorted at every
// depth (mirrors cc-switch's sort_json_keys: stable byte output regardless of
// insertion order, which makes diffs and auditing reliable).
func marshalSorted(v any) ([]byte, error) {
	return json.MarshalIndent(sortKeysValue(v), "", "  ")
}

// sortKeysValue recursively sorts map keys in JSON-shaped values. Non-map
// containers (slices) are traversed element-wise. Primitives pass through.
func sortKeysValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = sortKeysValue(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sortKeysValue(e)
		}
		return out
	default:
		return v
	}
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// claudeCodeAliasTiers lists claude code's model alias tiers in display order.
// Each tier maps to an env var ANTHROPIC_DEFAULT_{TIER}_MODEL (the real model id)
// and an optional _NAME (display name). The lowercased tier (opus/sonnet/...)
// is what claude code accepts as --model; the CLI maps it to the real model via
// these env vars, so AvailableModels must surface the alias, not the real model.
var claudeCodeAliasTiers = []struct {
	tier         string
	fallbackName string
}{
	{"OPUS", "Opus"},
	{"SONNET", "Sonnet"},
	{"HAIKU", "Haiku"},
	{"FABLE", "Fable"},
}

// aliasModelsFromEnv parses claude code's ANTHROPIC_DEFAULT_{TIER}_MODEL env
// vars into model options. Returns nil when no alias is configured. Used as the
// AvailableModels fallback so config/providers returns the user's real alias
// mapping (from ~/.claude/settings.json) instead of the hardcoded default list.
func aliasModelsFromEnv(env map[string]string) []core.ModelOption {
	var models []core.ModelOption
	for _, al := range claudeCodeAliasTiers {
		modelKey := "ANTHROPIC_DEFAULT_" + al.tier + "_MODEL"
		m, ok := env[modelKey]
		if !ok || m == "" {
			continue
		}
		name := env[modelKey+"_NAME"]
		if name == "" {
			name = al.fallbackName
		}
		models = append(models, core.ModelOption{
			Name: strings.ToLower(al.tier),
			Desc: name,
		})
	}
	return models
}

// modelsFromLiveConfig reads claude code model aliases from ~/.claude/settings.json.
// Fallback for AvailableModels when no provider is configured in memory and the
// Anthropic API is unreachable.
func (a *Agent) modelsFromLiveConfig() []core.ModelOption {
	cfg, err := a.ReadLiveProvider(context.Background())
	if err != nil || cfg.Env == nil {
		return nil
	}
	return aliasModelsFromEnv(cfg.Env)
}

// claudeLiveConfigFiles is the allowlist of editable config files exposed for
// whole-file editing. Currently just settings.json (~/.claude/settings.json),
// written at 0o644 like WriteLiveProvider.
var claudeLiveConfigFiles = []struct {
	name string
	file string
	perm os.FileMode
}{
	{"settings.json", "settings.json", 0o644},
}

// ListLiveConfigFiles returns every editable Claude Code config file with its
// current on-disk content. A missing file is surfaced with an empty Content
// (not an error), so a caller can seed a fresh install. Implements
// core.LiveConfigFileProvider.
func (a *Agent) ListLiveConfigFiles(_ context.Context) ([]core.LiveConfigFile, error) {
	dir, err := claudeConfigDir()
	if err != nil {
		return nil, err
	}
	out := make([]core.LiveConfigFile, 0, len(claudeLiveConfigFiles))
	for _, f := range claudeLiveConfigFiles {
		path := filepath.Join(dir, f.file)
		content, _ := os.ReadFile(path) // missing → empty content, not an error
		out = append(out, core.LiveConfigFile{
			Name:    f.name,
			Path:    path,
			Content: string(content),
		})
	}
	return out, nil
}

// WriteLiveConfigFile overwrites the named config file's entire content. The
// content is probe-parsed as JSON before it touches the disk. Unlike
// WriteLiveProvider, the bytes are written verbatim — key order, comments (if
// any survive jsonc parsing) and formatting are preserved, which is the whole
// point of the raw-file endpoint. Implements core.LiveConfigFileProvider.
func (a *Agent) WriteLiveConfigFile(_ context.Context, name string, content []byte) error {
	dir, err := claudeConfigDir()
	if err != nil {
		return err
	}

	var entry struct {
		name string
		file string
		perm os.FileMode
	}
	found := false
	for _, f := range claudeLiveConfigFiles {
		if f.name == name {
			entry.name, entry.file, entry.perm = f.name, f.file, f.perm
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("claudecode: config file %q not found", name)
	}

	// Probe-parse as JSON before persisting so a broken settings.json never
	// reaches the CLI. Empty content is allowed (clears the file).
	if len(bytes.TrimSpace(content)) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(content, &probe); err != nil {
			return fmt.Errorf("claudecode: invalid json for %s: %w", name, err)
		}
	}

	path := filepath.Join(dir, entry.file)
	if err := core.AtomicWriteFile(path, content, entry.perm); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", name, err)
	}
	return nil
}
