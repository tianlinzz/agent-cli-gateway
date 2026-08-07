// Command testworker is a stub worker executable used by the supervisor tests
// (and later integration tests) to exercise the worker RPC contract end to
// end without a real agent adapter. It serves the Worker gRPC service on the
// per-session Unix socket named by GW_WORKER_SOCKET and simulates an agent
// session whose behavior is selected by GW_TESTWORKER_BEHAVIOR:
//
//	(empty)|"echo"        persistent session; SendInput echoes a text event
//	"reject-start"        StartSession returns an error
//	"crash-on-start"      worker exits(9) while handling StartSession
//	"crash-after-start"   worker exits(1) ~300ms after StartSession succeeds
//	"ignore-close"        worker ignores SIGTERM and never finishes Close
//	"hang"                never serves anything (handshake-timeout tests)
//	"exit-immediately"    worker exits(3) before serving anything
//
// Every RPC is appended to the file named by GW_TESTWORKER_LOG (when set) so
// tests can assert deterministically what reached the worker. The worker also
// writes its own PID to GW_TESTWORKER_PIDFILE (when set) so tests can assert
// process-group reaping.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
)

func main() {
	socket := os.Getenv("GW_WORKER_SOCKET")
	if socket == "" {
		fmt.Fprintln(os.Stderr, "testworker: GW_WORKER_SOCKET is not set")
		os.Exit(2)
	}
	behavior := os.Getenv("GW_TESTWORKER_BEHAVIOR")
	logPath := os.Getenv("GW_TESTWORKER_LOG")
	pidfile := os.Getenv("GW_TESTWORKER_PIDFILE")
	if pidfile != "" {
		if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "testworker: write pidfile:", err)
			os.Exit(2)
		}
	}

	if behavior == "hang" {
		// Never serve anything: the supervisor's handshake must time out and
		// then reap us.
		time.Sleep(24 * time.Hour)
		os.Exit(0)
	}
	if behavior == "exit-immediately" {
		os.Exit(3)
	}
	if behavior == "ignore-close" {
		// Simulate a worker that cannot be stopped gracefully; the supervisor
		// must escalate to a process-group SIGKILL.
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	}

	h := newStubHandler(behavior, logPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.onClose = cancel // CloseSession closes the event stream and unblocks Serve

	if err := worker.Serve(ctx, worker.UnixSocket(socket), h); err != nil {
		fmt.Fprintln(os.Stderr, "testworker:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// stubHandler implements worker.Handler over a scripted event stream.
type stubHandler struct {
	mu       sync.Mutex
	behavior string
	logPath  string
	events   chan runtime.Event
	closed   bool
	onClose  func()
}

func newStubHandler(behavior, logPath string) *stubHandler {
	return &stubHandler{
		behavior: behavior,
		logPath:  logPath,
		events:   make(chan runtime.Event, 64),
	}
}

func (h *stubHandler) logf(format string, args ...any) {
	if h.logPath == "" {
		return
	}
	f, err := os.OpenFile(h.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

func (h *stubHandler) emit(ev runtime.Event) {
	h.logf("emit %s", ev.Type)
	select {
	case h.events <- ev:
	default:
	}
}

func (h *stubHandler) Health(_ context.Context) (string, error) {
	h.logf("health")
	if h.behavior == "fail-health" {
		return "", errors.New("stub: health failed")
	}
	return "testworker-1.0", nil
}

func (h *stubHandler) StartSession(_ context.Context, req worker.StartSessionReq) (string, error) {
	h.logf("start-session model=%s session=%s owner=%s workspace=%s", req.ModelID, req.SessionID, req.OwnerID, req.WorkspaceID)
	switch h.behavior {
	case "reject-start":
		return "", errors.New("stub: start rejected")
	case "crash-on-start":
		os.Exit(9) // kill the whole worker process mid-RPC
	case "crash-after-start":
		h.emit(runtime.Event{Type: runtime.EventStatus, Status: "started"})
		time.AfterFunc(300*time.Millisecond, func() { os.Exit(1) })
		return "persistent_process", nil
	}
	h.emit(runtime.Event{Type: runtime.EventStatus, Status: "started"})
	return "persistent_process", nil
}

func (h *stubHandler) SendInput(_ context.Context, input runtime.Input) error {
	h.logf("send-input messages=%d", len(input.Messages))
	var text string
	for i, m := range input.Messages {
		if i > 0 {
			text += "\n"
		}
		text += m.Content
	}
	h.emit(runtime.Event{Type: runtime.EventText, Text: "echo:" + text})
	h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	return nil
}

func (h *stubHandler) Events() <-chan runtime.Event {
	return h.events
}

func (h *stubHandler) Abort(_ context.Context) error {
	h.logf("abort")
	h.emit(runtime.Event{Type: runtime.EventStatus, Status: "aborted"})
	return nil
}

func (h *stubHandler) Close(_ context.Context) error {
	h.logf("close")
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	if h.behavior == "ignore-close" {
		// Refuse to terminate: the supervisor must escalate to SIGKILL.
		return nil
	}
	close(h.events)
	if h.onClose != nil {
		h.onClose()
	}
	return nil
}
