//go:build agent_ref

package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// Most of the codex MCP behaviour mirrors the live_config tests: temp home,
// round-trip, preserve-other-sections, idempotent delete. The distinct point is
// TOML [mcp_servers.*] round-tripping without clobbering other sections.

func TestCodex_ListMcpServers_EmptyWhenNoFile(t *testing.T) {
	withTempCodexHome(t, func(_ string, a *Agent) {
		got, err := a.ListMcpServers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})
}

func TestCodex_SaveMcpServer_RoundTrip(t *testing.T) {
	withTempCodexHome(t, func(_ string, a *Agent) {
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
			t.Fatal("fs not found after save")
		}
		if entry.Command != "npx" {
			t.Errorf("command = %q, want npx", entry.Command)
		}
		if len(entry.Args) != 3 || entry.Args[0] != "-y" {
			t.Errorf("args = %v", entry.Args)
		}
		if entry.Env["ROOT"] != "/tmp" {
			t.Errorf("env ROOT = %q", entry.Env["ROOT"])
		}
	})
}

func TestCodex_SaveMcpServer_PreservesModelProvider(t *testing.T) {
	// An existing [model_providers.openai] section + top-level model must
	// survive an MCP upsert (round-tripped through BurntSushi).
	withTempCodexHome(t, func(home string, a *Agent) {
		writeFile(t, filepath.Join(home, "config.toml"), `model = "gpt-5"

[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
`)

		err := a.SaveMcpServer(context.Background(), "fs", core.McpServerConfig{
			Command: "npx",
		})
		if err != nil {
			t.Fatal(err)
		}

		raw := readFileOrEmpty(t, filepath.Join(home, "config.toml"))
		if !strings.Contains(raw, `model = "gpt-5"`) {
			t.Errorf("top-level model dropped:\n%s", raw)
		}
		if !strings.Contains(raw, "model_providers.openai") {
			t.Errorf("model_providers.openai dropped:\n%s", raw)
		}
		if !strings.Contains(raw, "[mcp_servers.fs]") {
			t.Errorf("mcp_servers.fs not added:\n%s", raw)
		}
	})
}

func TestCodex_SaveMcpServer_UpdatesExistingEntry(t *testing.T) {
	withTempCodexHome(t, func(home string, a *Agent) {
		writeFile(t, filepath.Join(home, "config.toml"), `[mcp_servers.fs]
command = "old-cmd"
args = ["old"]
`)
		err := a.SaveMcpServer(context.Background(), "fs", core.McpServerConfig{
			Command: "new-cmd",
			Args:    []string{"new"},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.ListMcpServers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got["fs"].Command != "new-cmd" {
			t.Errorf("command = %q, want new-cmd", got["fs"].Command)
		}
	})
}

func TestCodex_DeleteMcpServer(t *testing.T) {
	withTempCodexHome(t, func(home string, a *Agent) {
		writeFile(t, filepath.Join(home, "config.toml"), `[mcp_servers.keep]
command = "a"

[mcp_servers.remove]
command = "b"
`)
		if err := a.DeleteMcpServer(context.Background(), "remove"); err != nil {
			t.Fatal(err)
		}
		got, err := a.ListMcpServers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got["remove"]; ok {
			t.Error("remove still present")
		}
		if _, ok := got["keep"]; !ok {
			t.Error("keep was deleted unexpectedly")
		}
	})
}

func TestCodex_DeleteMcpServer_Idempotent(t *testing.T) {
	withTempCodexHome(t, func(_ string, a *Agent) {
		if err := a.DeleteMcpServer(context.Background(), "ghost"); err != nil {
			t.Errorf("idempotent delete failed: %v", err)
		}
		// Even when there is no config.toml at all.
		if err := a.DeleteMcpServer(context.Background(), "ghost2"); err != nil {
			t.Errorf("idempotent delete on missing file failed: %v", err)
		}
	})
}

func TestCodex_SaveMcpServer_Validation(t *testing.T) {
	withTempCodexHome(t, func(_ string, a *Agent) {
		if err := a.SaveMcpServer(context.Background(), "bad", core.McpServerConfig{}); err == nil {
			t.Error("expected validation error for stdio without command")
		}
		if err := a.SaveMcpServer(context.Background(), "bad", core.McpServerConfig{Type: "http"}); err == nil {
			t.Error("expected validation error for http without url")
		}
		// http with url — OK
		if err := a.SaveMcpServer(context.Background(), "ok", core.McpServerConfig{Type: "http", URL: "https://mcp.example.com"}); err != nil {
			t.Errorf("valid http server rejected: %v", err)
		}
	})
}

func TestCodex_DeleteMcpServer_RemovesEmptySection(t *testing.T) {
	// When the last MCP server is deleted, the empty [mcp_servers] map should
	// be removed from config.toml so re-encoding doesn't write an empty table.
	withTempCodexHome(t, func(home string, a *Agent) {
		writeFile(t, filepath.Join(home, "config.toml"), `[mcp_servers.only]
command = "x"
`)
		if err := a.DeleteMcpServer(context.Background(), "only"); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "mcp_servers") {
			t.Errorf("empty mcp_servers section should be removed:\n%s", raw)
		}
	})
}

func TestNormalizeType(t *testing.T) {
	cases := map[string]string{
		"":        "stdio",
		"  ":      "stdio",
		"STDIO":   "stdio",
		"  Sse ":  "sse",
		"HTTP":    "http",
	}
	for in, want := range cases {
		if got := normalizeType(in); got != want {
			t.Errorf("normalizeType(%q) = %q, want %q", in, got, want)
		}
	}
}
