//go:build unix

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func signalProcessGroup(cmd *exec.Cmd) error {
	return signalUnixProcessGroup(cmd, syscall.SIGTERM)
}

func forceKillProcessGroup(cmd *exec.Cmd) error {
	return signalUnixProcessGroup(cmd, syscall.SIGKILL)
}

func signalUnixProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
