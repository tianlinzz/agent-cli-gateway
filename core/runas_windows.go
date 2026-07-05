//go:build windows

// Package core — runas_windows.go is the Windows stub for the spawn-as-different-
// user primitive defined in runas.go (which is //go:build !windows).
//
// Why a stub: the Unix implementation spawns child processes via
//
//	sudo -n -iu <target-user> -- <command>
//
// to achieve OS-user isolation for multi-tenant deployments. None of sudo,
// login shells, or setuid exist on Windows, so run_as_user is a no-op config
// knob here. Without this file the claudecode package (which references
// SpawnOptions/BuildSpawnCommand/...) would not compile on Windows, breaking
// the gateway build for Windows hosts entirely — see claudecode_test.go's
// TestWorkspaceAgentOptions_RoundTripsThroughNew comment which already
// anticipated this stub.
//
// Behaviour contract on Windows:
//   - SpawnOptions.IsolationMode() is always false. There is no RunAsUser
//     field because Windows cannot honour it.
//   - BuildSpawnCommand falls through to a plain exec.Command, identical to
//     the Unix non-isolated path.
//   - VerifyRunAsUserCheap returns a clear "unsupported on Windows" error so a
//     stray run_as_user value surfaces loudly instead of silently degrading.
//   - FilterEnvForSpawn returns env unchanged (no isolation = no filtering).
//
// All public symbols required by the claudecode package are defined here with
// the same signatures as runas.go so cross-platform code compiles unchanged.

package core

import (
	"context"
	"errors"
	"os/exec"
	"os/user"
	"sort"
)

// RunAsChdirEnv is defined for symbol parity with runas.go. It is never read
// on Windows because IsolationMode() is always false, but keeping the constant
// avoids "undefined" compile errors in callers that reference it defensively.
const RunAsChdirEnv = "CC_RUNAS_CHDIR"

// DefaultEnvAllowlist mirrors runas.go's value. Unused on Windows (no env
// filtering happens), but kept for symbol parity and to make future cross-
// platform tooling that reads it behave consistently.
var DefaultEnvAllowlist = []string{
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"LC_MESSAGES",
	"TERM",
}

// SpawnOptions controls how a command is spawned. On Windows the zero value
// (no isolation) is the only supported configuration; RunAsUser is omitted
// because Windows cannot spawn via sudo. The struct is kept field-compatible
// at the type level (EnvAllowlist / WorkDir) so callers that populate it from
// cross-platform config don't need build tags.
type SpawnOptions struct {
	RunAsUser    string   // always "" on Windows; kept for struct-literal compat
	EnvAllowlist []string // unused on Windows (no sudo boundary to cross)
	WorkDir      string   // honoured by BuildSpawnCommand as cmd.Dir
}

// IsolationMode is always false on Windows — there is no sudo, so the
// "spawn as a different OS user" story does not apply. Callers therefore
// always take the legacy direct-spawn branch, matching the Unix default when
// run_as_user is not configured.
func (o SpawnOptions) IsolationMode() bool { return false }

// mergedAllowlist is here for signature parity. It is unused on Windows but
// kept so any future cross-platform helper that calls it compiles.
func (o SpawnOptions) mergedAllowlist() []string {
	seen := make(map[string]struct{}, len(DefaultEnvAllowlist)+len(o.EnvAllowlist))
	for _, v := range DefaultEnvAllowlist {
		seen[v] = struct{}{}
	}
	for _, v := range o.EnvAllowlist {
		if v == "" {
			continue
		}
		seen[v] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// BuildSpawnCommand returns a plain *exec.Cmd on Windows. IsolationMode() is
// always false here, so the sudo-wrapping branch from runas.go never applies.
// WorkDir, if set, is applied as cmd.Dir by the caller (claudecode/session.go
// does this unconditionally after this returns).
func BuildSpawnCommand(ctx context.Context, opts SpawnOptions, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// FilterEnvForSpawn returns env unchanged on Windows. There is no sudo boundary
// to filter against.
func FilterEnvForSpawn(env []string, opts SpawnOptions) []string {
	return env
}

// SudoRunner is defined for symbol parity with runas.go. Its single method is
// never invoked on Windows because IsolationMode() is always false, but the
// type must exist so callers (claudecode/session.go references ExecSudoRunner{}
// in source) compile.
type SudoRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ExecSudoRunner is the no-op Windows counterpart of runas.go's runner. Its
// Run method returns a clear "unsupported" error: if anything ever routes here
// it means IsolationMode() somehow returned true, which should be impossible —
// surfacing the error makes such a regression immediately visible.
type ExecSudoRunner struct{}

func (ExecSudoRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return nil, errors.New("runas: sudo spawn is not supported on Windows")
}

// VerifyRunAsUserCheap returns an "unsupported" error on Windows. It is only
// reached when a caller has ignored IsolationMode()==false and tried to verify
// a run_as_user value anyway; treat that as a configuration error rather than
// silently passing.
func VerifyRunAsUserCheap(ctx context.Context, runner SudoRunner, runAsUser string) error {
	if runAsUser == "" {
		return errors.New("VerifyRunAsUserCheap: runAsUser is empty")
	}
	return errors.New("runas: run_as_user is not supported on Windows (remove the run_as_user setting from your config)")
}

// currentUsername returns the current login name, mirroring runas.go's helper.
// Currently uncalled on Windows (the only caller, runas_check.go, is
// Unix-only) but defined here for parity so a future cross-platform caller
// compiles without build tags.
func currentUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
