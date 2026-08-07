//go:build !windows

package worker

import "syscall"

// pidAlive reports whether a PID is still alive (signal 0 probe).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// killProcess sends SIGKILL to a single PID (used to simulate an nsjail
// wrapper crash in tests).
func killProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
