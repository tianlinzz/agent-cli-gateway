package archtest

import (
	"bufio"
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

func TestDependencyRules(t *testing.T) {
	root := moduleRoot(t)
	legacy := readAllowedLegacy(t, root)

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
				if !legacy[relative] {
					rejectImport(t, relative, path, "runtime", "adapters", "worker", "api", "config")
				}
			case "adapters":
				rejectImport(t, relative, path, "worker", "api")
			case "worker":
				rejectImport(t, relative, path, "agent/claudecode", "agent/codex", "agent/kimi", "adapters")
			}
		}
	})
}

func TestNoNewLegacySources(t *testing.T) {
	root := moduleRoot(t)
	legacy := readAllowedLegacy(t, root)
	needles := []string{"//go:build " + "agent_ref", modulePath + "/core"}

	walkGoFiles(t, root, func(file, relative string) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("read %s: %v", relative, err)
			return
		}
		for _, needle := range needles {
			if strings.Contains(string(data), needle) && !legacy[relative] {
				t.Errorf("new legacy source %s contains %q", relative, needle)
			}
		}
	})
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

func readAllowedLegacy(t *testing.T, root string) map[string]bool {
	t.Helper()
	allowed := make(map[string]bool)
	file, err := os.Open(filepath.Join(root, "internal", "archtest", "testdata", "allowed_legacy.txt"))
	if os.IsNotExist(err) {
		return allowed
	}
	if err != nil {
		t.Fatalf("open legacy allowlist: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			allowed[filepath.ToSlash(line)] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan legacy allowlist: %v", err)
	}
	return allowed
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
