//go:build linux

package nsjail

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// platformPreflight performs the linux-only capability checks: it pins the
// nsjail version and runs a minimal jail to verify that user namespaces,
// mount namespaces, and the base seccomp policy actually work on this host.
// Any failure is fail-closed.
func platformPreflight(ctx context.Context, iso config.IsolationConfig) error {
	// Validate the actual configured binary by executing a minimal jail. nsjail
	// does not provide a stable --version flag across upstream releases, so
	// version pinning is enforced at image/build time rather than guessed here.
	return minimalJail(ctx, iso)
}

// minimalJail runs `/bin/true` inside a minimal nsjail config that exercises
// the user namespace, the mount namespace, and the base seccomp whitelist. A
// clean exit (status 0) proves the namespaces and seccomp are available.
func minimalJail(ctx context.Context, iso config.IsolationConfig) error {
	tmp, err := os.MkdirTemp("", "nsjail-preflight-*")
	if err != nil {
		return fmt.Errorf("nsjail: preflight: mkdir temp: %w", err)
	}
	defer os.RemoveAll(tmp)

	ws := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return fmt.Errorf("nsjail: preflight: mkdir workspace: %w", err)
	}

	prof, err := Build(iso, SessionLayout{
		WorkspaceDir: ws,
		AgentHomeDir: ws,
		SocketDir:    ws,
		SocketPath:   filepath.Join(ws, "w.sock"),
		ParentPath:   os.Getenv("PATH"),
	}, "preflight")
	if err != nil {
		return fmt.Errorf("nsjail: preflight: build minimal profile: %w", err)
	}
	profilePath := filepath.Join(tmp, "profile.conf")
	if err := os.WriteFile(profilePath, []byte(prof.Config), 0o600); err != nil {
		return fmt.Errorf("nsjail: preflight: write minimal profile: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, iso.BinaryPath, "-Mo", "--config", profilePath, "--", "/bin/true")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nsjail: preflight: minimal jail failed (namespaces/seccomp unavailable?): %w; output: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
