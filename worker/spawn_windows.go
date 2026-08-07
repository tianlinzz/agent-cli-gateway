//go:build windows

package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// startCommand turns a command line into a supervised process on Windows.
// Windows has no POSIX process groups or signals; the closest equivalent is
// CREATE_NEW_PROCESS_GROUP, which makes the child the leader of its own
// console process group (so a future console Ctrl-Break could reach it).
// nsjail is Linux-only, so on Windows only the dev/test direct-spawn path is
// ever exercised.
func startCommand(_ context.Context, argv, env []string, logPath string) (*exec.Cmd, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("worker: spawn: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	out := (io.Writer)(os.Stderr)
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("worker: open session log %q: %w", logPath, err)
		}
		out = f
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("worker: start %q: %w", argv[0], err)
	}
	return cmd, nil
}

// killGroup force-kills the worker process tree on Windows. POSIX process
// groups and signals do not exist here; taskkill /T /F is the closest analog
// of signaling a process group (it terminates the process and its children).
// nsjail cannot run on Windows, so the direct worker is the only target.
func (ws *workerSession) killGroup(sig syscall.Signal) {
	for _, pid := range []int{ws.pgid, ws.workerPID} {
		if pid <= 0 {
			continue
		}
		if err := killWindowsTree(pid); err != nil {
			slog.Warn("worker: kill process tree", "pid", pid, "signal", sig, "error", err)
		}
	}
}

// killWindowsTree terminates a process and its whole subtree via taskkill.
func killWindowsTree(pid int) error {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill %d: %w: %s", pid, err, out)
	}
	return nil
}
