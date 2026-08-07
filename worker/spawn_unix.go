//go:build !windows

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

// startCommand turns a command line into a supervised process on unix-like
// hosts (linux, darwin). The child starts in its OWN process group (Setpgid),
// so the supervisor can signal -pgid to reach the whole tree (nsjail + worker
// + agent CLI). On Windows the equivalent lives in spawn_windows.go.
func startCommand(_ context.Context, argv, env []string, logPath string) (*exec.Cmd, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("worker: spawn: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

// killGroup signals the whole process group (nsjail + worker + agent CLI).
// The worker PID is signaled directly too as a defensive measure in case it
// escaped the group (e.g. a wrapper that calls setsid).
func (ws *workerSession) killGroup(sig syscall.Signal) {
	if ws.pgid > 0 {
		if err := syscall.Kill(-ws.pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("worker: kill process group", "pgid", ws.pgid, "signal", sig, "error", err)
		}
	}
	if ws.workerPID > 0 && ws.workerPID != ws.pgid {
		if err := syscall.Kill(ws.workerPID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("worker: signal worker pid", "pid", ws.workerPID, "signal", sig, "error", err)
		}
	}
}
