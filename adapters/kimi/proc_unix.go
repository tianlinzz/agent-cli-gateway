//go:build unix

// Process-group teardown helpers — migrated from the upstream
// agent/kimi/... pattern (agent/codex/proc_unix.go, originally cc-connect).
package kimi

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareCmdForKill(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// forceKillCmd SIGKILLs the whole process group of cmd (cmd is the group
// leader because prepareCmdForKill set Setpgid). EPERM is treated as "nothing
// left to signal", not as an error: on macOS, kill(-pgid, SIGKILL) returns
// EPERM when the group's only remaining members are zombies.
func forceKillCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	return nil
}
