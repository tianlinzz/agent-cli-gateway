//go:build unix

package process

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestForceKillTerminatesWholeProcessGroup(t *testing.T) {
	p, err := Start(context.Background(), Spec{
		Command: []string{"sh", "-c", "sleep 30 & wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ForceKill(); err != nil {
		t.Fatalf("force kill: %v", err)
	}
	if err := p.Wait(); err == nil {
		t.Fatal("wait error = nil, want killed process")
	}

	deadline := time.Now().Add(2 * time.Second)
	for processGroupExists(p.PID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupExists(p.PID()) {
		t.Fatalf("process group %d still exists", p.PID())
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func processGroupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
