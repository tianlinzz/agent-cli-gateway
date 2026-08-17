package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCommandSpecAcceptsArgvArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.toml")
	write := func(t *testing.T, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("array is canonical and the only argument-capable form", func(t *testing.T) {
		write(t, `
mode = "test"

[agents.codex]
enabled = false
command = ["/opt/agent tools/codex", "app-server", "--listen", "stdio://"]
`)
		cfg, err := LoadGateway(path)
		if err != nil {
			t.Fatal(err)
		}
		command := cfg.Agents["codex"].Command
		if command.IsLegacyString() {
			t.Fatal("array form must not be flagged legacy")
		}
		want := []string{"/opt/agent tools/codex", "app-server", "--listen", "stdio://"}
		if !reflect.DeepEqual(command.Argv(), want) {
			t.Fatalf("argv = %#v, want %#v", command.Argv(), want)
		}
	})

	t.Run("legacy string is one executable token, not shell syntax", func(t *testing.T) {
		write(t, `
mode = "test"

[agents.codex]
enabled = false
command = "codex --flag"
`)
		cfg, err := LoadGateway(path)
		if err != nil {
			t.Fatal(err)
		}
		command := cfg.Agents["codex"].Command
		if !command.IsLegacyString() {
			t.Fatal("string form must be flagged legacy for the deprecation warning")
		}
		// The whole string — spaces and all — is a single executable path.
		// It will fail the availability probe unless an executable with that
		// literal name exists, which is the point: arguments must migrate to
		// the array form (roadmap Phase 3.3, code-review Phase 3 P1).
		want := []string{"codex --flag"}
		if !reflect.DeepEqual(command.Argv(), want) {
			t.Fatalf("argv = %#v, want the whole string as ONE executable token %#v", command.Argv(), want)
		}
	})

	t.Run("legacy string with spaces stays one token", func(t *testing.T) {
		write(t, `
mode = "test"

[agents.codex]
enabled = false
command = "/opt/agent tools/codex"
`)
		cfg, err := LoadGateway(path)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"/opt/agent tools/codex"}
		if !reflect.DeepEqual(cfg.Agents["codex"].Command.Argv(), want) {
			t.Fatalf("argv = %#v, want %#v (single executable path)", cfg.Agents["codex"].Command.Argv(), want)
		}
	})

	t.Run("non-string array element rejected", func(t *testing.T) {
		write(t, `
mode = "test"

[agents.codex]
enabled = false
command = ["codex", 3]
`)
		if _, err := LoadGateway(path); err == nil {
			t.Fatal("numeric command element must fail config load")
		}
	})

	t.Run("numeric command rejected", func(t *testing.T) {
		write(t, `
mode = "test"

[agents.codex]
enabled = false
command = 3
`)
		if _, err := LoadGateway(path); err == nil {
			t.Fatal("numeric command must fail config load")
		}
	})
}

// TestLoadGatewayWarnsOnLegacyCommandString_OF14 asserts the deprecation
// contract: the string form still loads (one deprecation window) and is
// flagged so LoadGateway warns, while the argv array form is not flagged.
func TestLoadGatewayWarnsOnLegacyCommandString_OF14(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.toml")
	if err := os.WriteFile(path, []byte(`
mode = "test"

[agents.kimi]
enabled = false
command = "kimi"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Agents["kimi"].Command.IsLegacyString() {
		t.Fatal("string command must be flagged legacy")
	}
	if !reflect.DeepEqual(cfg.Agents["kimi"].Command.Argv(), []string{"kimi"}) {
		t.Fatalf("argv = %#v", cfg.Agents["kimi"].Command.Argv())
	}
}

// TestLoadGateway_LegacyStringIsNotTokenized guards against the removed
// shell-like splitter creeping back: a quoted legacy string is ONE literal
// executable token with no quote processing.
func TestLoadGateway_LegacyStringIsNotTokenized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.toml")
	if err := os.WriteFile(path, []byte(`
mode = "test"

[agents.codex]
enabled = false
command = 'codex --flag "quoted"'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`codex --flag "quoted"`}
	if got := cfg.Agents["codex"].Command.Argv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want the literal string as one token %#v (no shell tokenization)", got, want)
	}
}
