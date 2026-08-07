//go:build agent_ref

package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// --- pure-function tests for wrapForWindows / helpers (run on any OS) --------

func wrapEntry(cmd string, args ...string) map[string]any {
	entry := map[string]any{"command": cmd}
	if len(args) > 0 {
		anyArgs := make([]any, len(args))
		for i, a := range args {
			anyArgs[i] = a
		}
		entry["args"] = anyArgs
	}
	return entry
}

func TestWrapForWindows_NonWindows_NoOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test only meaningful off windows")
	}
	entry := wrapEntry("npx", "-y", "server")
	cmd, args := wrapForWindows(entry)
	if cmd != "npx" {
		t.Errorf("cmd = %q, want npx", cmd)
	}
	if len(args) != 2 || args[0] != "-y" || args[1] != "server" {
		t.Errorf("args = %v, want [-y server]", args)
	}
}

func TestCommandStem_StripsSuffixAndPath(t *testing.T) {
	cases := map[string]string{
		"npx":                    "npx",
		"NPM":                    "npm",
		"C:\\bin\\node.exe":      "node",
		"/usr/local/bin/npx":     "npx",
		"yarn.CMD":               "yarn",
		"":                       "",
		"python":                 "python",
	}
	for in, want := range cases {
		if got := commandStem(in); got != want {
			t.Errorf("commandStem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsWslPath(t *testing.T) {
	cases := map[string]bool{
		`\\wsl$\Ubuntu`:         true,
		`\\wsl.localhost\Ubuntu`: true,
		`\\WSL$\foo`:             true, // case-insensitive
		`C:\Users\me`:            false,
		`/usr/local/bin`:         false,
		`\\server\share`:         false,
	}
	for in, want := range cases {
		if got := isWslPath(in); got != want {
			t.Errorf("isWslPath(%q) = %v, want %v", in, got, want)
		}
	}
}

// wrapForWindowsAssert is a helper that, on Windows, checks the rewrite
// happened; off Windows it checks the input passes through unchanged.
func wrapForWindowsAssert(t *testing.T, entry map[string]any, wantCmd string, wantArgs []any) {
	t.Helper()
	cmd, args := wrapForWindows(entry)
	if cmd != wantCmd {
		t.Errorf("cmd = %q, want %q", cmd, wantCmd)
		return
	}
	if !equalAnySlice(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
}

func equalAnySlice(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWrapForWindows_WrapsNpxOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test only meaningful on windows")
	}
	// npx -y server  →  cmd /c npx -y server
	wrapForWindowsAssert(t,
		wrapEntry("npx", "-y", "server"),
		"cmd",
		[]any{"/c", "npx", "-y", "server"},
	)
}

func TestWrapForWindows_WrapsEvenWithoutArgs(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test only meaningful on windows")
	}
	// bare npx  →  cmd /c npx
	wrapForWindowsAssert(t,
		wrapEntry("npx"),
		"cmd",
		[]any{"/c", "npx"},
	)
}

func TestWrapForWindows_DoesNotDoubleWrap(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test only meaningful on windows")
	}
	// already cmd — no re-wrap
	entry := map[string]any{"command": "cmd"}
	entry["args"] = []any{"/c", "npx"}
	wrapForWindowsAssert(t, entry, "cmd", []any{"/c", "npx"})
}

func TestWrapForWindows_SkipsUnlistedCommand(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test only meaningful on windows")
	}
	// python is not in windowsWrapCommands — no wrapping
	wrapForWindowsAssert(t,
		wrapEntry("python", "-m", "server"),
		"python",
		[]any{"-m", "server"},
	)
}

func TestWrapForWindows_WslPathExempted(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test only meaningful on windows")
	}
	// A WSL UNC path command should NOT be wrapped even if it's npx.
	entry := wrapEntry(`\\wsl$\Ubuntu\usr\bin\npx`, "-y", "server")
	wrapForWindowsAssert(t, entry, `\\wsl$\Ubuntu\usr\bin\npx`, []any{"-y", "server"})
}

func TestWrapForWindows_SseHttpNotWrapped(t *testing.T) {
	// Remote servers never have a "command" wrapped — they use url.
	entry := map[string]any{"type": "sse", "url": "https://mcp.example.com/sse"}
	cmd, args := wrapForWindows(entry)
	if cmd != "" || args != nil {
		t.Errorf("sse server should not be wrapped: cmd=%q args=%v", cmd, args)
	}
}

// --- integration tests (touch ~/.claude.json via temp HOME) -----------------

func writeClaudeJSON(t *testing.T, homeDir string, v map[string]any) {
	t.Helper()
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(homeDir, ".claude.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readClaudeJSONMap(t *testing.T, homeDir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(homeDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClaude_ListMcpServers_EmptyWhenNoFile(t *testing.T) {
	withTempHome(t, func(_ string) {
		a := &Agent{}
		got, err := a.ListMcpServers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})
}

func TestClaude_SaveMcpServer_RoundTrip(t *testing.T) {
	withTempHome(t, func(_ string) {
		a := &Agent{}
		cfg := core.McpServerConfig{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
			Env:     map[string]string{"ROOT": "/tmp"},
		}
		if err := a.SaveMcpServer(context.Background(), "fs", cfg); err != nil {
			t.Fatal(err)
		}

		got, err := a.ListMcpServers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		entry, ok := got["fs"]
		if !ok {
			t.Fatal("fs server not found after save")
		}
		// On Windows the command is wrapped; check accordingly.
		if runtime.GOOS == "windows" {
			if entry.Command != "cmd" {
				t.Errorf("cmd = %q, want cmd (wrapped on windows)", entry.Command)
			}
			wantArgs := []string{"/c", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"}
			if len(entry.Args) != len(wantArgs) {
				t.Errorf("args = %v, want %v", entry.Args, wantArgs)
			}
		} else {
			if entry.Command != "npx" {
				t.Errorf("cmd = %q, want npx", entry.Command)
			}
			if len(entry.Args) != 3 {
				t.Errorf("args len = %d, want 3", len(entry.Args))
			}
		}
		if entry.Env["ROOT"] != "/tmp" {
			t.Errorf("env ROOT = %q, want /tmp", entry.Env["ROOT"])
		}
	})
}

func TestClaude_SaveMcpServer_PreservesOtherFields(t *testing.T) {
	// ~/.claude.json holds onboarding state, project history etc. — must
	// survive an MCP edit.
	withTempHome(t, func(homeDir string) {
		writeClaudeJSON(t, homeDir, map[string]any{
			"hasCompletedOnboarding": true,
			"mcpServers": map[string]any{
				"existing": map[string]any{"command": "python", "args": []any{"-m", "old"}},
			},
			"someAppData": map[string]any{"k": "v"},
		})

		a := &Agent{}
		err := a.SaveMcpServer(context.Background(), "new", core.McpServerConfig{
			Command: "node",
		})
		if err != nil {
			t.Fatal(err)
		}

		root := readClaudeJSONMap(t, homeDir)
		if root["hasCompletedOnboarding"] != true {
			t.Error("hasCompletedOnboarding was clobbered")
		}
		if root["someAppData"] == nil {
			t.Error("someAppData was clobbered")
		}
		servers, _ := root["mcpServers"].(map[string]any)
		if _, ok := servers["existing"]; !ok {
			t.Error("existing mcp server was dropped")
		}
		if _, ok := servers["new"]; !ok {
			t.Error("new mcp server not added")
		}
	})
}

func TestClaude_SaveMcpServer_StripsUIFields(t *testing.T) {
	// UI metadata (enabled/source/id/...) must NOT be persisted.
	withTempHome(t, func(homeDir string) {
		a := &Agent{}
		err := a.SaveMcpServer(context.Background(), "fs", core.McpServerConfig{
			Command: "npx",
			Env:     map[string]string{"X": "1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Re-read raw to confirm no UI fields leaked (they never go in via the
		// typed struct anyway, but guard against future refactors).
		root := readClaudeJSONMap(t, homeDir)
		servers, _ := root["mcpServers"].(map[string]any)
		entry, _ := servers["fs"].(map[string]any)
		for _, bad := range []string{"enabled", "source", "id", "name", "description"} {
			if _, leaked := entry[bad]; leaked {
				t.Errorf("UI field %q leaked into .claude.json", bad)
			}
		}
	})
}

func TestClaude_DeleteMcpServer(t *testing.T) {
	withTempHome(t, func(homeDir string) {
		writeClaudeJSON(t, homeDir, map[string]any{
			"mcpServers": map[string]any{
				"keep":   map[string]any{"command": "a"},
				"remove": map[string]any{"command": "b"},
			},
		})
		a := &Agent{}
		if err := a.DeleteMcpServer(context.Background(), "remove"); err != nil {
			t.Fatal(err)
		}
		got, err := a.ListMcpServers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got["remove"]; ok {
			t.Error("remove still present after delete")
		}
		if _, ok := got["keep"]; !ok {
			t.Error("keep was deleted unexpectedly")
		}
	})
}

func TestClaude_DeleteMcpServer_Idempotent(t *testing.T) {
	withTempHome(t, func(_ string) {
		a := &Agent{}
		// deleting a name that doesn't exist (and file may not even exist)
		if err := a.DeleteMcpServer(context.Background(), "ghost"); err != nil {
			t.Errorf("idempotent delete failed: %v", err)
		}
	})
}

func TestClaude_SaveMcpServer_Validation(t *testing.T) {
	withTempHome(t, func(_ string) {
		a := &Agent{}
		// stdio without command
		if err := a.SaveMcpServer(context.Background(), "bad", core.McpServerConfig{}); err == nil {
			t.Error("expected validation error for stdio without command")
		}
		// sse without url
		if err := a.SaveMcpServer(context.Background(), "bad", core.McpServerConfig{Type: "sse"}); err == nil {
			t.Error("expected validation error for sse without url")
		}
		// sse with url — OK
		if err := a.SaveMcpServer(context.Background(), "ok", core.McpServerConfig{Type: "sse", URL: "https://mcp.example.com/sse"}); err != nil {
			t.Errorf("valid sse server rejected: %v", err)
		}
	})
}
