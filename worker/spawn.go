package worker

import (
	"context"
	"fmt"
	"os/exec"
)

// spawnSpec carries everything the process spawner needs for one session.
type spawnSpec struct {
	// isolated marks the session as requiring the nsjail wrapper. Only the
	// test runtime mode may spawn directly.
	isolated bool
	// nsjailBinary is the path to the nsjail executable (isolated sessions).
	nsjailBinary string
	// profilePath is the generated nsjail config file (isolated sessions).
	profilePath string
	// workerExe is the worker child executable.
	workerExe string
	// env is the environment for the spawned process.
	env []string
	// logPath is the per-session file worker stdout/stderr is written to.
	// When empty the supervisor's own stderr is used.
	logPath string
}

// spawnFunc starts the session worker process. The returned *exec.Cmd is
// already started and is the direct child of the supervisor. The child runs
// in its own process group (Setpgid), so the supervisor can signal -pgid to
// reach the whole tree (nsjail + worker + agent CLI).
type spawnFunc func(ctx context.Context, spec spawnSpec) (*exec.Cmd, error)

// defaultSpawner selects the platform-appropriate spawn strategy: a direct
// spawn only when isolation is disabled (test profile only), otherwise the
// nsjail-wrapped spawn — a real nsjail fork on linux, a fail-closed error
// elsewhere.
func defaultSpawner(cfg Config) spawnFunc {
	if !cfg.Isolation.Required {
		return spawnDirect
	}
	return spawnJailed
}

// buildNsjailCommand assembles the exact argv used to fork the worker inside
// nsjail: `nsjail -Mo --config <profile> -- <worker> <args...>`. nsjail runs
// in ONCE mode (-Mo): one nsjail process per session, one jail, lifecycle 1:1
// with the session. nsjail itself is never part of the RPC contract — this
// argv is a worker-side startup detail.
func buildNsjailCommand(nsjailBinary, profilePath, workerExe string) []string {
	return []string{nsjailBinary, "-Mo", "--config", profilePath, "--", workerExe}
}

// buildDirectCommand assembles the argv for a direct (test-only, isolation
// disabled) spawn.
func buildDirectCommand(workerExe string) []string {
	return []string{workerExe}
}

// spawnDirect starts the worker directly, without nsjail. It is a fail-closed
// seam: it REFUSES to fork when the session requires isolation (spec.isolated),
// so no reachable code path can spawn an unsandboxed worker when
// Isolation.Required=true in a non-test mode (spec.isolated mirrors
// cfg.Isolation.Required at the single call site in StartSession). Only the
// test profile (isolation disabled → spec.isolated=false) may spawn directly.
func spawnDirect(ctx context.Context, spec spawnSpec) (*exec.Cmd, error) {
	if spec.isolated {
		return nil, fmt.Errorf("worker: spawn: refusing unsandboxed direct spawn: session requires nsjail isolation")
	}
	return startCommand(ctx, buildDirectCommand(spec.workerExe), spec.env, spec.logPath)
}

// startCommand is defined in the platform files:
//   - spawn_unix.go (!windows): the worker starts in its own process group
//     (Setpgid), so the supervisor can signal -pgid to reach the whole tree
//     (nsjail + worker + agent CLI).
//   - spawn_windows.go (windows): CREATE_NEW_PROCESS_GROUP, since Windows has
//     no POSIX process groups (nsjail is Linux-only; Windows is dev-only).
//
// It deliberately does NOT use CommandContext: process lifetime (and
// process-group signals) is owned entirely by the supervisor, so a
// short-lived caller context must never kill the child.
//
// Worker output is written to logPath (or the supervisor's stderr) as a plain
// *os.File. It must NOT be piped through exec.Cmd: a piped Stdout/Stderr makes
// cmd.Wait() block until the pipe reaches EOF, which — when a grandchild (the
// agent CLI, or an orphaned worker after the nsjail wrapper dies) inherits the
// pipe descriptors — would stall the monitor goroutine that is responsible for
// reaping the process group.
