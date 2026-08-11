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
			case "adapters":
				rejectImport(t, relative, path, "worker", "api")
			case "worker":
				rejectImport(t, relative, path, "agent/claudecode", "agent/codex", "agent/kimi", "adapters")
			}
		}
	})
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
