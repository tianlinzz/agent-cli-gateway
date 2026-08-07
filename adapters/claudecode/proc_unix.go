//go:build unix

// Process-group teardown helpers — migrated verbatim from the upstream
// agent/claudecode/proc_unix.go (originally cc-connect).
package claudecode

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// sigterm is the graceful-stop signal for Close()'s phase 2.
var sigterm = syscall.SIGTERM

// prepareCmdForKill puts the spawned child into its own process group so that
// the entire descendant tree can be terminated with a single signal aimed at
// the negative PID. Without this, the adapter can only signal the direct
// child (e.g. the `claude` CLI), leaving any grandchildren (MCP server
// processes) as orphans.
func prepareCmdForKill(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the entire process group rooted at cmd.
// Returns nil if the group is already gone.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil &&
		!errors.Is(err, os.ErrProcessDone) &&
		!errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// forceKillCmd SIGKILLs the entire process group rooted at cmd. Use this
// as the last-resort escalation when graceful shutdown has timed out.
func forceKillCmd(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGKILL)
}
