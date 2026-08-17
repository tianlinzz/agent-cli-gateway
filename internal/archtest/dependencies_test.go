package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/tianlinzz/agent-cli-gateway"
const ccConnectBaseline = "3fc360ee6acc9bab13ab1b48ddde3af44062903b"
const hermesBaseline = "9d6c5a920c773f86fad9ea16528212faeaa21815"
const kimiCodeBaseline = "2acf22f66e15361d9804d9014d58ad68a9383caf"

func TestDependencyRules(t *testing.T) {
	root := moduleRoot(t)

	walkGoFiles(t, root, func(file, relative string) {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse imports %s: %v", relative, err)
			return
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("unquote import in %s: %v", relative, err)
				continue
			}
			switch topLevelPackage(relative) {
			case "runtime":
				rejectImport(t, relative, path, "agent", "adapters", "worker", "api")
			case "agent":
				rejectImport(t, relative, path, "runtime", "adapters", "worker", "api", "config")
				// agent/<name> packages may only share code through the
				// sanctioned agent/* foundations: events (D1), process,
				// protocol, and rpcsession (D2). One agent importing another
				// is forbidden.
				if seg := agentSubPackage(relative); seg != "" {
					if path == modulePath+"/agent" || strings.HasPrefix(path, modulePath+"/agent/") {
						switch path {
						case modulePath + "/agent/process", modulePath + "/agent/protocol", modulePath + "/agent/events", modulePath + "/agent/rpcsession":
						default:
							t.Errorf("forbidden dependency: %s imports %s (agent/<name> may only import agent/events, agent/process, agent/protocol, agent/rpcsession, stdlib)", relative, path)
						}
					}
				}
			case "adapters":
				rejectImport(t, relative, path, "worker", "api")
			case "worker":
				rejectImport(t, relative, path, "agent/claudecode", "agent/codex", "agent/kimi", "adapters")
			}
		}
	})
}

// agentSubPackage returns the second path segment for files under agent/<name>
// (e.g. "codex" for agent/codex/session.go), or "" for the foundation
// packages themselves (agent/process, agent/protocol, agent/events).
func agentSubPackage(relative string) string {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) < 2 || parts[0] != "agent" {
		return ""
	}
	switch parts[1] {
	case "process", "protocol", "events", "rpcsession":
		return ""
	}
	return parts[1]
}

func TestNoLegacyAgentSources(t *testing.T) {
	root := moduleRoot(t)
	needles := []string{"//go:build " + "agent_ref", modulePath + "/core"}

	walkGoFiles(t, root, func(file, relative string) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			return
		}
		for _, needle := range needles {
			if strings.Contains(string(data), needle) {
				t.Errorf("legacy source remains: %s contains %q", relative, needle)
			}
		}
	})
}

func TestNoResumePerTurnCompatibility(t *testing.T) {
	root := moduleRoot(t)
	forbidden := []string{
		"Lifecycle" + "ResumePerTurn",
		"resume" + "_per_turn",
		"CC_GATEWAY" + "_CODEX_" + "BACKEND",
		"Probe" + "Flags(",
		"Flag" + "Support",
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == ".superpowers" || filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "docs", "superpowers")) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".toml" && ext != ".md" && ext != ".sh" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		relative, _ := filepath.Rel(root, path)
		for _, needle := range forbidden {
			if strings.Contains(string(data), needle) {
				t.Errorf("obsolete lifecycle compatibility remains: %s contains %q", filepath.ToSlash(relative), needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoEnvVarConfigRoundtrip enforces the O-F08 contract: agent deployment
// config reaches adapter factories through runtime.AdapterConfig. The old
// CC_GATEWAY-prefixed environment-variable bridge (typed config -> os.Setenv
// -> adapter env re-parse) must not come back. Scanned sources exclude docs,
// where the removed names survive as historical references.
func TestNoEnvVarConfigRoundtrip(t *testing.T) {
	root := moduleRoot(t)
	forbidden := "CC_GATEWAY" + "_"
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == ".superpowers" || filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "docs")) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".toml" && ext != ".sh" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), forbidden) {
			relative, _ := filepath.Rel(root, path)
			t.Errorf("env-var config roundtrip remains: %s contains %q", filepath.ToSlash(relative), forbidden)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdaptersContainNoNativeExecution(t *testing.T) {
	root := moduleRoot(t)
	needles := []string{"os/exec", "exec.Command", "bufio.Scanner", "json.Unmarshal"}
	walkGoFiles(t, filepath.Join(root, "adapters"), func(file, relative string) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			return
		}
		for _, needle := range needles {
			if strings.Contains(string(data), needle) {
				t.Errorf("native execution remains in adapter: %s contains %q", relative, needle)
			}
		}
	})
}

func TestProvenance(t *testing.T) {
	root := moduleRoot(t)
	for _, agent := range []string{"claudecode", "codex", "kimi"} {
		relative := filepath.Join("adapters", agent, "SOURCE.md")
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			continue
		}
		content := string(data)
		for _, required := range []string{
			"https://github.com/chenhg5/cc-connect",
			ccConnectBaseline,
			"Upstream source files",
			"Migrated behaviors",
			"Material local modifications",
			"Local regression tests",
			"agent/" + agent + "/",
		} {
			if !strings.Contains(content, required) {
				t.Errorf("%s missing %q", relative, required)
			}
		}
	}

	for relative, required := range map[string][]string{
		"adapters/codex/SOURCE.md": {hermesBaseline, "app-server", "MIT"},
		"adapters/kimi/SOURCE.md":  {kimiCodeBaseline, "ACP", "MIT"},
	} {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			continue
		}
		for _, needle := range required {
			if !strings.Contains(string(data), needle) {
				t.Errorf("%s missing native protocol provenance %q", relative, needle)
			}
		}
	}

	license := filepath.Join(root, "LICENSES", "cc-connect-MIT.txt")
	data, err := os.ReadFile(license)
	if err != nil {
		t.Fatalf("read upstream license: %v", err)
	}
	for _, required := range []string{
		"MIT License",
		"README.md",
		ccConnectBaseline,
		"standalone LICENSE file is absent",
	} {
		if !strings.Contains(string(data), required) {
			t.Errorf("upstream license missing %q", required)
		}
	}
}

func TestActiveDocumentation(t *testing.T) {
	root := moduleRoot(t)
	for _, relative := range []string{"README.md", "AGENTS.md", "CLAUDE.md"} {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			continue
		}
		content := string(data)
		for _, forbidden := range []string{"migration-reference", "agent_ref", "never build with"} {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s contains stale documentation %q", relative, forbidden)
			}
		}
		for _, required := range []string{
			"agent/<name>",
			"adapters/<name>",
			"OpenAI HTTP/SSE",
			"Worker Supervisor/RPC",
			"one active turn per session",
			"same workspace",
		} {
			if !strings.Contains(content, required) {
				t.Errorf("%s missing final architecture statement %q", relative, required)
			}
		}
	}

	// The rewrite boundary must hold in the contributor-facing documents too
	// (code-review Phase 3 P2): CONTRIBUTING.md and the root CHANGELOG must
	// not describe cc-connect's IM-platform capabilities or point contributors
	// at the upstream repo as if it were this project. Pre-rewrite history is
	// archived under docs/ with an explicit provenance header.
	for _, relative := range []string{"CONTRIBUTING.md", "CHANGELOG.md"} {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			continue
		}
		content := string(data)
		for _, forbidden := range []string{
			"Contributing to cc-connect",
			"chenhg5/cc-connect",
			"Feishu", "Telegram", "Discord", "WeChat", "Weixin", "Matrix",
			"Reasonix",
		} {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s still documents the pre-rewrite IM project: contains %q (archive under docs/ instead)", relative, forbidden)
			}
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func walkGoFiles(t *testing.T, root string, visit func(file, relative string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", ".worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visit(path, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

func topLevelPackage(relative string) string {
	if before, _, ok := strings.Cut(filepath.ToSlash(relative), "/"); ok {
		return before
	}
	return ""
}

func rejectImport(t *testing.T, file, imported string, forbidden ...string) {
	t.Helper()
	for _, prefix := range forbidden {
		if imported == modulePath+"/"+prefix || strings.HasPrefix(imported, modulePath+"/"+prefix+"/") {
			t.Errorf("forbidden dependency: %s imports %s", file, imported)
		}
	}
}

// TestNoTimeAfterInProduction enforces the O-B6 timer discipline: production
// code must use stoppable timers (time.NewTimer/time.NewTicker with Stop)
// instead of time.After, whose timer cannot be stopped and is only reclaimed
// after it fires. Test files are exempt.
func TestNoTimeAfterInProduction(t *testing.T) {
	root := moduleRoot(t)
	walkGoFiles(t, root, func(file, relative string) {
		if strings.HasSuffix(relative, "_test.go") {
			return
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			return
		}
		if strings.Contains(string(data), "time.After(") {
			t.Errorf("production code must use a stoppable timer (time.NewTimer), found time.After: %s", relative)
		}
	})
}
