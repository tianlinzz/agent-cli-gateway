// Package process provides Agent-agnostic child-process mechanics.
package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unicode/utf8"
)

// Spec describes one native Agent CLI process. Command contains the executable
// followed by its arguments. Env overrides the inherited process environment.
type Spec struct {
	Command []string
	Dir     string
	Env     []string
	Stdin   bool
}

// Process owns a started child, its process group, pipes, and reaping result.
// The process is reaped even when the caller never invokes Wait.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr lockedBuffer

	stdinMu     sync.Mutex
	stdinClosed bool
	done        chan struct{}
	waitErr     error
}

// Start launches a process and begins asynchronous reaping.
func Start(ctx context.Context, spec Spec) (*Process, error) {
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		return nil, fmt.Errorf("agent process: empty command")
	}
	if ctx == nil {
		return nil, fmt.Errorf("agent process: nil context")
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = MergeEnv(os.Environ(), spec.Env)
	prepareProcessGroup(cmd)

	p := &Process{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &p.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent process: stdout pipe for %q: %w", spec.Command[0], err)
	}
	p.stdout = stdout
	if spec.Stdin {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			_ = stdout.Close()
			return nil, fmt.Errorf("agent process: stdin pipe for %q: %w", spec.Command[0], err)
		}
		p.stdin = stdin
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = p.CloseStdin()
		return nil, fmt.Errorf("agent process: start %q in %q: %w", spec.Command[0], spec.Dir, err)
	}

	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.ForceKill()
		case <-p.done:
		}
	}()
	return p, nil
}

// MergeEnv returns base with overrides applied by case-insensitive key.
// Existing key positions are preserved and new keys are appended.
func MergeEnv(base, overrides []string) []string {
	merged := append([]string(nil), base...)
	for _, override := range overrides {
		key := envKey(override)
		replaced := false
		for i, current := range merged {
			if strings.EqualFold(envKey(current), key) {
				merged[i] = override
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, override)
		}
	}
	return merged
}

func envKey(entry string) string {
	if i := strings.IndexByte(entry, '='); i >= 0 {
		return entry[:i]
	}
	return entry
}

// Stdin returns the configured stdin pipe, or nil when Spec.Stdin was false.
func (p *Process) Stdin() io.WriteCloser {
	if p == nil {
		return nil
	}
	return p.stdin
}

// CloseStdin closes stdin once.
func (p *Process) CloseStdin() error {
	if p == nil {
		return nil
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdin == nil || p.stdinClosed {
		return nil
	}
	p.stdinClosed = true
	return p.stdin.Close()
}

// Stdout returns the streaming stdout pipe.
func (p *Process) Stdout() io.ReadCloser {
	if p == nil {
		return nil
	}
	return p.stdout
}

// StderrString returns the stderr captured so far.
func (p *Process) StderrString() string {
	if p == nil {
		return ""
	}
	return p.stderr.String()
}

// PID returns the direct child's process identifier, or zero before start.
func (p *Process) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Wait blocks until the child has been reaped. It is safe for concurrent and
// repeated callers, which all observe the same result.
func (p *Process) Wait() error {
	if p == nil {
		return nil
	}
	<-p.done
	return p.waitErr
}

// SignalGraceful requests termination of the entire process group.
func (p *Process) SignalGraceful() error {
	if p == nil {
		return nil
	}
	return signalProcessGroup(p.cmd)
}

// ForceKill forcibly terminates the entire process group.
func (p *Process) ForceKill() error {
	if p == nil {
		return nil
	}
	return forceKillProcessGroup(p.cmd)
}

// defaultStderrCap bounds captured child stderr to the recent tail. A chatty
// CLI run with --verbose over a long-lived session could otherwise exhaust
// worker memory.
const defaultStderrCap = 256 * 1024

// stderrTruncationMarker is prepended to the captured stderr when the buffer
// dropped older output, so a caller can tell the diagnostics are partial.
const stderrTruncationMarker = "...[stderr truncated]...\n"

// lockedBuffer is a bounded, mutex-guarded buffer that retains the most recent
// tail of what is written to it. After every Write the retained content is at
// most max bytes (defaultStderrCap when max <= 0) and starts on a UTF-8 rune
// boundary. A single Write larger than max never buffers the whole input: only
// its final max bytes are kept. String prepends a truncation marker when older
// output was dropped.
type lockedBuffer struct {
	mu        sync.Mutex
	b         []byte
	max       int
	truncated bool
}

func (b *lockedBuffer) cap() int {
	if b.max > 0 {
		return b.max
	}
	return defaultStderrCap
}

// trimToRuneStart drops leading bytes that cannot begin a UTF-8 rune so the
// retained tail is valid UTF-8 at its start.
func trimToRuneStart(p []byte) []byte {
	for len(p) > 0 && !utf8.RuneStart(p[0]) {
		p = p[1:]
	}
	return p
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	max := b.cap()
	switch {
	case len(p) >= max:
		// The whole retained tail comes from this write; never buffer all of p.
		b.truncated = true
		b.b = append([]byte(nil), trimToRuneStart(p[len(p)-max:])...)
	default:
		b.b = append(b.b, p...)
		if len(b.b) > max {
			b.truncated = true
			b.b = append([]byte(nil), trimToRuneStart(b.b[len(b.b)-max:])...)
		}
	}
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncated {
		return string(b.b)
	}
	return stderrTruncationMarker + string(b.b)
}
