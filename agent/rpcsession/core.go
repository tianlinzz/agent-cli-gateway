// Package rpcsession is the shared lifecycle skeleton for persistent
// JSON-RPC agent CLI sessions (D2): codex's app-server and kimi's ACP sessions
// embed Core and keep only their protocol specifics — handshake, turn model,
// notification dispatch, abort mechanics, usage decoding.
//
// Core owns the mechanical lifecycle both sessions had previously duplicated
// (and that had already drifted): process launch, the monitor/teardown
// goroutine, event channel ownership, notification-overflow markers,
// failed-start cleanup, abort escalation, and the Close sequence with its
// CloseTimeout settle budget. It is name-agnostic and imports only the
// sanctioned agent foundations (agent/events, agent/process, agent/protocol)
// plus the standard library.
package rpcsession

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/agent/events"
	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
	agentprotocol "github.com/tianlinzz/agent-cli-gateway/agent/protocol"
)

// Core is the embedded lifecycle skeleton. The embedding session constructs it
// with New, launches the CLI through Launch, performs its native handshake,
// then calls FinishStart to arm the monitor.
type Core struct {
	// Adapter labels the agent in logs ("codex", "kimi").
	Adapter string
	// ProcessLabel names the CLI in exit diagnostics ("app-server", "ACP").
	ProcessLabel string
	// CloseTimeout bounds Close (and the sessions' abort settle waits)
	// before SIGKILL escalation. The per-turn deadline is NOT Core's concern:
	// the worker boundary owns it (O-F09b).
	CloseTimeout time.Duration

	process *agentprocess.Process
	rpc     *agentprotocol.JSONRPCClient
	events  chan events.Event
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	nativeID  atomic.Value
	alive     atomic.Bool
	closing   atomic.Bool
	closeOnce sync.Once

	// BeforeExit runs inside monitor after the process is reaped and before
	// the terminal-error decision; kimi uses it to wait for in-flight prompt
	// goroutines. Optional.
	BeforeExit func()

	// errorf builds the embedding package's protocol error (each agent keeps
	// its own sentinel prefix). Required.
	errorf func(format string, args ...any) error
}

// New allocates the core. adapter/processLabel label diagnostics; closeTimeout
// bounds teardown; errorf wraps core-built errors in the agent's protocol
// error shape.
func New(adapter, processLabel string, closeTimeout time.Duration, errorf func(format string, args ...any) error) *Core {
	return &Core{
		Adapter:      adapter,
		ProcessLabel: processLabel,
		CloseTimeout: closeTimeout,
		events:       make(chan events.Event, 64),
		done:         make(chan struct{}),
		errorf:       errorf,
	}
}

// Launch starts the CLI process and the JSON-RPC client over its stdio. The
// session keeps its own session context (derived from ctx) so a per-request
// cancellation never tears down the persistent CLI. On failure the caller has
// nothing to clean up; a successful Launch followed by a failed handshake is
// unwound with CleanupFailedStart.
func (c *Core) Launch(ctx context.Context, spec agentprocess.Spec, maxFrame int, reverse agentprotocol.ReverseHandler, notify agentprotocol.NotificationHandler, classify agentprotocol.NotifyClassifier) error {
	if ctx == nil {
		return c.errorf("nil context")
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	proc, err := agentprocess.Start(sessionCtx, spec)
	if err != nil {
		cancel()
		return c.errorf("start %s: %w", c.ProcessLabel, err)
	}
	c.process = proc
	c.ctx = sessionCtx
	c.cancel = cancel
	c.rpc = agentprotocol.NewJSONRPCClient(proc.Stdin(), proc.Stdout(), maxFrame, reverse, notify, c.OnNotifyOverflow, classify)
	return nil
}

// FinishStart marks the session alive and arms the monitor goroutine. Call
// exactly once, after the native handshake succeeded.
func (c *Core) FinishStart() {
	c.alive.Store(true)
	go c.monitor()
}

// CleanupFailedStart tears down a session whose handshake failed: kill the
// process, reap it, and leave the event channel unconsumed.
func (c *Core) CleanupFailedStart() {
	c.closing.Store(true)
	c.cancel()
	_ = c.rpc.Close()
	_ = c.process.ForceKill()
	_ = c.process.Wait()
}

// Emit publishes one native event. The send is done-guarded: teardown unblocks
// a stalled consumer instead of leaking the emitter.
func (c *Core) Emit(event events.Event) {
	select {
	case c.events <- event:
	case <-c.ctx.Done():
	}
}

// OnNotifyOverflow surfaces a notification-truncation marker when the JSON-RPC
// reader dropped notifications because a stalled consumer saturated the queue.
// It is best-effort: if the consumer is still stalled the marker waits (done-
// guarded) like any other event.
func (c *Core) OnNotifyOverflow(dropped int) {
	c.Emit(events.Event{Kind: events.EventReasoning, Reasoning: &events.Reasoning{Text: fmt.Sprintf("[%d notifications truncated]", dropped)}})
}

// monitor reaps the CLI exactly once, emits the exit error (unless closing),
// then drains emitters and closes the event channel.
func (c *Core) monitor() {
	<-c.rpc.Done()
	waitErr := c.process.Wait()
	if c.BeforeExit != nil {
		c.BeforeExit()
	}
	c.alive.Store(false)
	if !c.closing.Load() {
		message := strings.TrimSpace(c.process.StderrString())
		if message == "" {
			if waitErr != nil {
				message = waitErr.Error()
			} else if err := c.rpc.Err(); err != nil && !errors.Is(err, io.EOF) {
				message = err.Error()
			}
		}
		if message != "" {
			c.Emit(events.Event{Kind: events.EventError, Err: c.errorf("%s exited: %s", c.ProcessLabel, message)})
		}
	}
	c.closeOnce.Do(func() {
		// Cancel first so any emitter blocked in Emit's select returns, then
		// wait for the JSON-RPC notification drain goroutine to stop calling
		// notify before closing the events channel. Otherwise a drain
		// mid-emit would send on a closed channel.
		c.cancel()
		if ch := c.rpc.NotifyDone(); ch != nil {
			<-ch
		}
		close(c.events)
		close(c.done)
	})
}

// AbortEscalation is the converged abort fallback: when a native abort cannot
// settle within its bounds, kill and reap the whole process. Both agents use
// the same escalation; only the cause differs.
func (c *Core) AbortEscalation(cause error) error {
	c.closing.Store(true)
	c.alive.Store(false)
	c.cancel()
	_ = c.rpc.Close()
	_ = c.process.ForceKill()
	_ = c.process.Wait()
	return c.errorf("abort: %w", cause)
}

// Close terminates and reaps the persistent CLI once, bounded by CloseTimeout
// (then 2×CloseTimeout for the kill-to-reap wait).
func (c *Core) Close(ctx context.Context) error {
	if !c.alive.Swap(false) {
		select {
		case <-c.done:
			return nil
		default:
		}
	}
	c.closing.Store(true)
	c.cancel()
	_ = c.rpc.Close()
	timer := time.NewTimer(c.CloseTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		_ = c.process.ForceKill()
		return c.errorf("close wait: %w", ctx.Err())
	case <-timer.C:
		_ = c.process.ForceKill()
		reapTimer := time.NewTimer(c.CloseTimeout)
		defer reapTimer.Stop()
		select {
		case <-c.done:
			return nil
		case <-reapTimer.C:
			return c.errorf("close timed out after %s", 2*c.CloseTimeout)
		}
	}
}

// Alive reports whether the session is still usable.
func (c *Core) Alive() bool { return c.alive.Load() }

// Events returns the session's native event channel. Core owns the channel and
// closes it at teardown.
func (c *Core) Events() <-chan events.Event { return c.events }

// Done is closed when the session has fully terminated.
func (c *Core) Done() <-chan struct{} { return c.done }

// RPC exposes the JSON-RPC client for the embedding session's protocol calls.
func (c *Core) RPC() *agentprotocol.JSONRPCClient { return c.rpc }

// Ctx is the session lifetime context (cancelled at teardown).
func (c *Core) Ctx() context.Context { return c.ctx }

// NativeID returns the resumable native conversation identifier.
func (c *Core) NativeID() string {
	id, _ := c.nativeID.Load().(string)
	return id
}

// SetNativeID records the resumable native conversation identifier.
func (c *Core) SetNativeID(id string) { c.nativeID.Store(id) }

// PID returns the CLI process identifier (0 before launch).
func (c *Core) PID() int { return c.process.PID() }
