// Package workspace resolves opaque client-supplied workspace IDs to
// controlled directories under a configured root. Resolution is a security
// boundary: no input can ever produce a path outside the root, because the
// directory is bind-mounted into the agent's nsjail sandbox by the worker
// layer (task 3). This package imports only the standard library.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Sentinel errors callers can match with errors.Is.
var (
	// ErrInvalidOwnerID is returned when the owner id is empty or contains
	// characters that could affect the resolved path (separators, NUL, dot
	// segments).
	ErrInvalidOwnerID = errors.New("workspace: invalid owner id")
	// ErrInvalidWorkspaceID is returned when the workspace id is empty or
	// contains characters that could affect the resolved path.
	ErrInvalidWorkspaceID = errors.New("workspace: invalid workspace id")
	// ErrPathEscape is returned when the candidate or symlink-resolved path
	// falls outside the workspace root. This is the fail-closed escape signal.
	ErrPathEscape = errors.New("workspace: resolved path escapes workspace root")
)

// Resolver maps (ownerID, workspaceID) pairs to controlled absolute
// directories under a fixed root. It is safe for concurrent use.
//
// Layout: <root>/<ownerID>/<workspaceID>. Scoping by owner means the same
// opaque workspace_id used by two tenants resolves to two non-overlapping
// directories, so tenants can never collide on or observe each other's files.
//
// Symlink policy (fail-closed): the root is canonicalized (EvalSymlinks) once
// at construction. At resolve time the lexical candidate is verified against
// the root with filepath.Rel, then the directory is created and re-verified
// AFTER EvalSymlinks, so a symlink planted inside the root that points outside
// it is rejected. A bind-mount of an escaped path is therefore impossible.
// Residual TOCTOU (a symlink swapped in between resolve and bind-mount) is
// closed by the nsjail mount namespace, which is the second boundary (task 3).
type Resolver struct {
	// root is the canonical (symlink-resolved) absolute workspace root.
	root string
}

// NewResolver builds a Resolver rooted at root. The root must already exist as
// a directory; a missing root is a configuration error and is rejected here so
// the server fails closed at startup rather than lazily creating host paths.
// root may be relative (it is absolutized against the process cwd) but never
// empty. The stored root is the symlink-resolved canonical path.
func NewResolver(root string) (*Resolver, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace: new resolver: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: new resolver: %w", err)
	}
	abs = filepath.Clean(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: new resolver: root %q: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("workspace: new resolver: root %q is not a directory", abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: new resolver: resolve root symlinks: %w", err)
	}
	return &Resolver{root: real}, nil
}

// Root returns the canonical absolute workspace root this resolver governs.
func (r *Resolver) Root() string {
	return r.root
}

// Resolve maps an opaque workspace_id to the absolute controlled directory
// under the root, creating it (and the owner directory) on demand with 0700
// permissions. The returned path is always inside Root and always absolute.
//
// ownerID and workspaceID are opaque tokens: they must be non-empty, must not
// contain path separators ('/' or the Windows '\' as a robustness check), must
// not be "." or "..", and must not contain a NUL byte. URL-encoded traversal
// sequences (e.g. "..%2f") are treated as literal characters, never decoded,
// so they are inert. Containment is verified both lexically (before mkdir) and
// after symlink resolution (after mkdir).
func (r *Resolver) Resolve(ownerID, workspaceID string) (string, error) {
	if err := validateToken(ownerID, ErrInvalidOwnerID); err != nil {
		return "", err
	}
	if err := validateToken(workspaceID, ErrInvalidWorkspaceID); err != nil {
		return "", err
	}

	candidate := filepath.Join(r.root, ownerID, workspaceID)
	if err := ensureInside(r.root, candidate); err != nil {
		return "", err
	}

	if err := os.MkdirAll(candidate, 0o700); err != nil {
		return "", fmt.Errorf("workspace: resolve %q: mkdir: %w", workspaceID, err)
	}

	// Fail-closed symlink policy: resolve the real path and re-verify. If the
	// directory (or an ancestor) is a symlink escaping the root, reject.
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve %q: eval symlinks: %w", workspaceID, err)
	}
	if err := ensureInside(r.root, real); err != nil {
		return "", fmt.Errorf("workspace: resolve %q: %w", workspaceID, err)
	}
	return real, nil
}

// validateToken rejects tokens that could alter the resolved path. Separators
// are rejected outright so a single token can never combine into a traversal,
// an absolute path, or a Windows-style path; containment checks below remain as
// defense in depth rather than the primary guard. Leading/trailing whitespace
// is rejected so a whitespace-padded traversal like " .. " can never mask a
// dot segment.
func validateToken(token string, sentinel error) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w: empty", sentinel)
	}
	if token != strings.TrimSpace(token) {
		return fmt.Errorf("%w: leading or trailing whitespace", sentinel)
	}
	if strings.ContainsRune(token, 0) {
		return fmt.Errorf("%w: contains NUL byte", sentinel)
	}
	if strings.ContainsAny(token, `/\\`) {
		return fmt.Errorf("%w: path separators are not allowed", sentinel)
	}
	if token == "." || token == ".." {
		return fmt.Errorf("%w: dot segments are not allowed", sentinel)
	}
	return nil
}

// ensureInside fails closed unless path is strictly inside root. A path equal
// to root, a ".." prefix, or an absolute relative result all count as escapes.
func ensureInside(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("workspace: compute relative path: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %q", ErrPathEscape, path)
	}
	return nil
}
