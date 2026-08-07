//go:build windows

package worker

// pidAlive reports whether a PID is still alive. On Windows there is no
// POSIX signal-0 probe; report true for any positive PID (tests that assert
// reaping use the worker lifecycle, not this probe).
func pidAlive(pid int) bool {
	return pid > 0
}

// killProcess force-kills a PID on Windows (best-effort; used to simulate an
// nsjail wrapper crash in tests, which only runs on unix hosts anyway).
func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	return killWindowsTree(pid)
}
