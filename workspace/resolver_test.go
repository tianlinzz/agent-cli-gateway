package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/workspace"
)

// assertInsideRoot verifies that the resolved path is absolute and that its
// relative position under root cannot escape the root (not "..", not a
// ".."-prefix, not absolute). The reference root is canonicalized the same way
// the resolver canonicalizes it (EvalSymlinks) so the comparison holds on hosts
// where the configured root itself sits behind a symlink (e.g. macOS /var).
func assertInsideRoot(t *testing.T, root, path string) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("resolved path %q is not absolute", path)
	}
	rel, err := filepath.Rel(canonical, path)
	if err != nil {
		t.Fatalf("Rel(%q, %q): %v", canonical, path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("resolved path %q escapes root %q (rel=%q)", path, canonical, rel)
	}
}

func newTestResolver(t *testing.T) (*workspace.Resolver, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "wsroot")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir wsroot: %v", err)
	}
	r, err := workspace.NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver(%q): %v", root, err)
	}
	return r, root
}

func TestResolverResolvesValidID(t *testing.T) {
	r, root := newTestResolver(t)

	got, err := r.Resolve("alice", "my-workspace")
	if err != nil {
		t.Fatalf("Resolve(alice, my-workspace): %v", err)
	}
	assertInsideRoot(t, root, got)

	// The resolved directory must actually exist on disk and be a directory.
	st, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat resolved dir: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("resolved path %q is not a directory", got)
	}

	// Resolving the same ID again must be idempotent.
	again, err := r.Resolve("alice", "my-workspace")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if got != again {
		t.Fatalf("Resolve not idempotent: %q != %q", got, again)
	}
}

func TestResolverRejectsInvalidIDs(t *testing.T) {
	r, _ := newTestResolver(t)

	cases := []struct {
		name  string
		owner string
		id    string
	}{
		// Path traversal via relative dot segments.
		{"dotdot", "alice", ".."},
		{"dotdot slash", "alice", "../"},
		{"dotdot with name", "alice", "../etc"},
		{"nested traversal", "alice", "a/../../etc"},
		{"trailing traversal", "alice", "ok/.."},
		{"single dot", "alice", "."},
		{"dotdot with spaces", "alice", " .. "},

		// Absolute paths must never be accepted.
		{"absolute unix", "alice", "/etc"},
		{"absolute nested", "alice", "/var/tmp/x"},
		{"double slash", "alice", "//server/share"},

		// Windows-style separators and drive letters (robustness check).
		{"windows backslash", "alice", `..\..\etc`},
		{"windows drive", "alice", `C:\Windows\System32`},
		{"windows drive lowercase", "alice", `c:/Windows`},

		// Embedded NUL must be rejected outright.
		{"embedded null", "alice", "a\x00b"},

		// Empty IDs are never valid.
		{"empty owner", "", "ws1"},
		{"empty workspace", "alice", ""},
		{"blank workspace", "alice", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Resolve(tc.owner, tc.id); err == nil {
				t.Fatalf("Resolve(%q, %q) succeeded, want error", tc.owner, tc.id)
			}
		})
	}
}

// TestResolverEncodedTraversalStaysInsideRoot proves that URL-encoded traversal
// attempts are inert: the resolver treats the id as an opaque token and never
// decodes it, so even a hostile id cannot move the resolved directory outside
// the root. This is the "..%2f" regression case.
func TestResolverEncodedTraversalStaysInsideRoot(t *testing.T) {
	r, root := newTestResolver(t)

	for _, id := range []string{
		"..%2f",
		"..%2f..%2fetc",
		"%2e%2e%2f",
		"..%252f",
		"..%00",
	} {
		got, err := r.Resolve("alice", id)
		if err != nil {
			t.Fatalf("Resolve(alice, %q): %v", id, err)
		}
		assertInsideRoot(t, root, got)
	}
}

func TestResolverRejectsMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := workspace.NewResolver(missing); err == nil {
		t.Fatalf("NewResolver(%q) succeeded, want root-not-exists error", missing)
	}
}

func TestResolverRejectsNonDirectoryRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := workspace.NewResolver(file); err == nil {
		t.Fatalf("NewResolver(%q) succeeded, want not-a-directory error", file)
	}
}

func TestResolverRejectsEmptyRoot(t *testing.T) {
	if _, err := workspace.NewResolver(""); err == nil {
		t.Fatal("NewResolver(\"\") succeeded, want empty-root error")
	}
	if _, err := workspace.NewResolver("   "); err == nil {
		t.Fatal("NewResolver(\"   \") succeeded, want empty-root error")
	}
}

// TestResolverScopesByOwner is the owner-binding regression: the same opaque
// workspace_id under different owners must resolve to different, non-overlapping
// directories, and both must stay inside the root. No cross-owner collision and
// no cross-owner path can ever be produced.
func TestResolverScopesByOwner(t *testing.T) {
	r, root := newTestResolver(t)

	a, err := r.Resolve("alice", "default")
	if err != nil {
		t.Fatalf("Resolve(alice, default): %v", err)
	}
	b, err := r.Resolve("bob", "default")
	if err != nil {
		t.Fatalf("Resolve(bob, default): %v", err)
	}
	assertInsideRoot(t, root, a)
	assertInsideRoot(t, root, b)

	if a == b {
		t.Fatalf("different owners resolved to the same directory %q", a)
	}
	if strings.HasPrefix(b, a+string(filepath.Separator)) {
		t.Fatalf("bob's workspace %q lives inside alice's workspace %q", b, a)
	}
}

// TestResolverRejectsSymlinkEscape pins the fail-closed symlink policy: a
// workspace or owner directory that is a symlink pointing outside the root must
// be rejected at resolve time, so a symlink planted in the root can never be
// bind-mounted as a host path.
func TestResolverRejectsSymlinkEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wsroot")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir wsroot: %v", err)
	}
	r, err := workspace.NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	// A directory outside the root that an attacker's symlink would point at.
	outside := filepath.Join(t.TempDir(), "outside-host-path")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, "alice"), 0o700); err != nil {
		t.Fatalf("mkdir alice: %v", err)
	}
	// Symlink escape via the workspace leaf.
	if err := os.Symlink(outside, filepath.Join(root, "alice", "evil")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := r.Resolve("alice", "evil"); err == nil {
		t.Fatal("Resolve through a workspace symlink escaped the root and was not rejected")
	}

	// Symlink escape via the owner component.
	if err := os.Symlink(outside, filepath.Join(root, "mallory")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := r.Resolve("mallory", "ws1"); err == nil {
		t.Fatal("Resolve through an owner symlink escaped the root and was not rejected")
	}
}

// TestResolver_RejectsPathEqualToRoot pins the fail-closed rule that a path
// equal to the root is itself an escape. filepath.Rel(root, root) returns "."
// (not ".."), so the old containment check missed it: a symlink under <owner>/
// pointing back at the root resolved to the root directory itself, which would
// let a bind-mount expose every tenant's workspace. Resolve must reject it.
func TestResolver_RejectsPathEqualToRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wsroot")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir wsroot: %v", err)
	}
	r, err := workspace.NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, "alice"), 0o700); err != nil {
		t.Fatalf("mkdir alice: %v", err)
	}
	// A workspace symlink pointing back at the root itself. The resolved path
	// equals the root, so it must be rejected as an escape.
	if err := os.Symlink(root, filepath.Join(root, "alice", "back-to-root")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := r.Resolve("alice", "back-to-root"); !errors.Is(err, workspace.ErrPathEscape) {
		t.Fatalf("Resolve of a path equal to the root: got %v, want ErrPathEscape", err)
	}
}

func TestResolverConcurrentResolve(t *testing.T) {
	r, root := newTestResolver(t)

	const workers = 8
	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = r.Resolve("alice", "shared")
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != first {
			t.Fatalf("goroutine %d resolved %q, want %q", i, results[i], first)
		}
		assertInsideRoot(t, root, results[i])
	}
}
