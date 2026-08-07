//go:build !linux

package worker

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// spawnJailed on non-linux hosts (darwin) fails closed: a real nsjail cannot
// run here, and the supervisor must NEVER silently fork a worker without the
// sandbox. The dev/prod profile therefore refuses to start sessions on this
// host. Unit tests exercise the full nsjail-wrapped lifecycle through an
// injected stub spawner that forks a fake nsjail script; the real nsjail path
// runs on Linux (CI, task 7).
func spawnJailed(_ context.Context, spec spawnSpec) (*exec.Cmd, error) {
	return nil, fmt.Errorf(
		"worker: nsjail isolation requires a linux host (running on %s); refusing to spawn session worker without the sandbox",
		runtime.GOOS,
	)
}
