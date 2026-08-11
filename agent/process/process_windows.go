//go:build windows

package process

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func prepareProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func signalProcessGroup(cmd *exec.Cmd) error {
	return taskkill(cmd, false)
}

func forceKillProcessGroup(cmd *exec.Cmd) error {
	return taskkill(cmd, true)
}

func taskkill(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	args := []string{"/T"}
	if force {
		args = append(args, "/F")
	}
	args = append(args, "/PID", strconv.Itoa(cmd.Process.Pid))
	output, err := exec.Command("taskkill", args...).CombinedOutput()
	if err == nil || taskkillProcessMissing(output) {
		return nil
	}
	if force {
		if killErr := cmd.Process.Kill(); killErr == nil || errors.Is(killErr, os.ErrProcessDone) {
			return nil
		}
	}
	return fmt.Errorf("taskkill: %w: %s", err, safeTaskkillOutput(output))
}

func taskkillProcessMissing(output []byte) bool {
	lower := bytes.ToLower(output)
	return bytes.Contains(lower, []byte("there is no running instance")) || bytes.Contains(lower, []byte("not found"))
}

func safeTaskkillOutput(output []byte) string {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return "(empty output)"
	}
	return trimmed
}
