package server

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveWorkDirInCwd runs resolveWorkDir with cwd temporarily changed to base,
// so tests are deterministic regardless of where `go test` runs.
func resolveWorkDirInCwd(t *testing.T, base, workDir string) string {
	t.Helper()
	// Resolve symlinks so macOS /private/var == /var comparisons match.
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	base = real
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return resolveWorkDir(workDir)
}

// TestResolveWorkDir rules:
//   empty        → cwd
//   relative     → joined under cwd
//   escapes cwd  → cwd (treated as not provided)
//   absolute     → NOT honored, treated like relative and joined (must stay in cwd)
func TestResolveWorkDir(t *testing.T) {
	base := t.TempDir()
	// Resolve symlinks once so expected values match resolveWorkDir's output.
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	base = real

	tests := []struct {
		name    string
		input   string
		wantSub string // suffix expected under base; "" means want == base
		wantEq  bool   // when true, want == base (cwd)
	}{
		{"empty returns cwd", "", "", true},
		{"relative name", "alice", filepath.Join(base, "alice"), false},
		{"relative nested", "alice/proj", filepath.Join(base, "alice", "proj"), false},
		{"escape rejected", "../etc", "", true},
		{"deep escape rejected", "a/../../../etc", "", true},
		{"dot stays in cwd", ".", base, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWorkDirInCwd(t, base, tt.input)
			want := base
			if !tt.wantEq {
				want = tt.wantSub
			}
			if got != want {
				t.Fatalf("resolveWorkDir(%q) = %q, want %q", tt.input, got, want)
			}
		})
	}
}

// TestApplyAgentConfig_CreatesNestedWorkDir: f1-web 传相对 "alice/proj"，
// 网关在 cwd 下创建该嵌套目录。
func TestApplyAgentConfig_CreatesNestedWorkDir(t *testing.T) {
	base := t.TempDir()
	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(base, "alice", "proj")
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatal("precondition failed")
	}

	agent := newMockAgent("claudecode")
	req := CreateSessionRequest{Agent: "claudecode", WorkDir: "alice/proj"}
	applyAgentConfig(agent, req)

	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		t.Fatalf("workDir should be created as a directory: %v", err)
	}
}
