package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

	t.Run("array is canonical", func(t *testing.T) {
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

	t.Run("legacy string splits shell-like and stays accepted", func(t *testing.T) {
		write(t, `
mode = "test"
[agents.codex]
enabled = false
command = "/opt/agent\\ tools/codex --quiet"
`)
		cfg, err := LoadGateway(path)
		if err != nil {
			t.Fatal(err)
		}
		command := cfg.Agents["codex"].Command
		if !command.IsLegacyString() {
			t.Fatal("string form must be flagged legacy for the deprecation warning")
		}
		want := []string{"/opt/agent tools/codex", "--quiet"}
		if !reflect.DeepEqual(command.Argv(), want) {
			t.Fatalf("argv = %#v, want %#v", command.Argv(), want)
		}
	})

	t.Run("unterminated quote fails config load", func(t *testing.T) {
		write(t, `
mode = "test"
[agents.codex]
enabled = false
command = 'codex --flag "oops'
`)
		if _, err := LoadGateway(path); err == nil || !strings.Contains(err.Error(), "unterminated") {
			t.Fatalf("err = %v, want unterminated quote failure", err)
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
}

func TestSplitCommandString(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    []string
		wantErr bool
	}{
		{name: "plain", command: "codex app-server", want: []string{"codex", "app-server"}},
		{name: "extra whitespace", command: "  codex\t--flag\n value  ", want: []string{"codex", "--flag", "value"}},
		{name: "double quoted path with spaces", command: `"/opt/agent tools/codex" --quiet`, want: []string{"/opt/agent tools/codex", "--quiet"}},
		{name: "single quoted path with spaces", command: `'/opt/agent tools/codex' --quiet`, want: []string{"/opt/agent tools/codex", "--quiet"}},
		{name: "backslash escape", command: `/opt/agent\ tools/codex --quiet`, want: []string{"/opt/agent tools/codex", "--quiet"}},
		{name: "escaped quote inside double quotes", command: `"say \"hi\""`, want: []string{`say "hi"`}},
		{name: "empty string", command: "", want: nil},
		{name: "unterminated double quote", command: `"codex`, wantErr: true},
		{name: "unterminated single quote", command: `'codex`, wantErr: true},
		{name: "trailing backslash", command: `codex \`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitCommandString(tc.command)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SplitCommandString(%q) = %#v, want error", tc.command, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitCommandString(%q): %v", tc.command, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitCommandString(%q) = %#v, want %#v", tc.command, got, tc.want)
			}
		})
	}
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
