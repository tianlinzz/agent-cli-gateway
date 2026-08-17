package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildArgsUsesPersistentAppServer(t *testing.T) {
	got := BuildArgs(Options{}, "")
	want := []string{"app-server", "--listen", "stdio://"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestSessionUsesOneAppServerForMultipleTurns(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "rpc.jsonl")
	session := startTestSession(t, map[string]string{"CODEX_TEST_LOG": logFile})

	for _, prompt := range []string{"first", "tool"} {
		if err := session.Send(context.Background(), Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		events := readCodexTurn(t, session.Events())
		if eventOfKind(events, EventFinish) == nil {
			t.Fatalf("events = %#v", events)
		}
		if reasoning := eventOfKind(events, EventReasoning); reasoning == nil || reasoning.Reasoning == nil || reasoning.Reasoning.Text != "safe reasoning summary" {
			t.Fatalf("reasoning summary = %#v", reasoning)
		}
	}

	entries := readRPCLog(t, logFile)
	assertMethodCount(t, entries, "initialize", 1)
	assertMethodCount(t, entries, "thread/start", 1)
	assertMethodCount(t, entries, "thread/resume", 0)
	assertMethodCount(t, entries, "turn/start", 2)
	if uniquePIDs(entries) != 1 {
		t.Fatalf("RPCs used multiple processes: %#v", entries)
	}
	if session.NativeSessionID() != "thread-native-1" {
		t.Fatalf("native session ID = %q", session.NativeSessionID())
	}
}

func TestSessionResumesThreadOnceDuringStartup(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "rpc.jsonl")
	session := startTestSessionWithOptions(t, Options{ResumeID: "thread-existing"}, map[string]string{"CODEX_TEST_LOG": logFile})
	if err := session.Send(context.Background(), Input{Prompt: "hello"}); err != nil {
		t.Fatal(err)
	}
	readCodexTurn(t, session.Events())
	entries := readRPCLog(t, logFile)
	assertMethodCount(t, entries, "thread/start", 0)
	assertMethodCount(t, entries, "thread/resume", 1)
	assertMethodCount(t, entries, "turn/start", 1)
}

func TestToolEventsHaveStableIDsAndSafeReasoningSummary(t *testing.T) {
	session := startTestSession(t, nil)
	if err := session.Send(context.Background(), Input{Prompt: "tool"}); err != nil {
		t.Fatal(err)
	}
	events := readCodexTurn(t, session.Events())
	use := eventOfKind(events, EventToolUse)
	result := eventOfKind(events, EventToolResult)
	if use == nil || result == nil || use.Tool == nil || result.Tool == nil || use.Tool.ID != "item-tool-1" || result.Tool.ID != use.Tool.ID {
		t.Fatalf("tool events = %#v", events)
	}
	if reasoning := eventOfKind(events, EventReasoning); reasoning == nil || reasoning.Reasoning == nil || reasoning.Reasoning.Text != "safe reasoning summary" {
		t.Fatalf("reasoning summary = %#v", reasoning)
	}
	if got := eventKindCount(events, EventToolResult); got != 1 {
		t.Fatalf("tool result count = %d, want 1; events=%#v", got, events)
	}
}

func TestAbortInterruptsTurnAndPreservesAppServer(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "rpc.jsonl")
	readyFile := filepath.Join(t.TempDir(), "ready")
	session := startTestSession(t, map[string]string{"CODEX_TEST_LOG": logFile, "CODEX_TEST_READY": readyFile})
	if err := session.Send(context.Background(), Input{Prompt: "block"}); err != nil {
		t.Fatal(err)
	}
	waitFile(t, readyFile)
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if events := readCodexTurn(t, session.Events()); eventOfKind(events, EventFinish) == nil {
		t.Fatalf("aborted turn events = %#v", events)
	}
	if !session.Alive() {
		t.Fatal("app-server was closed by turn abort")
	}
	if err := session.Send(context.Background(), Input{Prompt: "after"}); err != nil {
		t.Fatal(err)
	}
	readCodexTurn(t, session.Events())
	entries := readRPCLog(t, logFile)
	assertMethodCount(t, entries, "turn/interrupt", 1)
	assertMethodCount(t, entries, "turn/start", 2)
	if uniquePIDs(entries) != 1 {
		t.Fatalf("abort restarted app-server: %#v", entries)
	}
}

// TestAbortWaitsForTurnIDPublication_OB4 is a regression test for O-B4:
// when Abort is called while the turn id has not yet been published (abort
// racing turn-start), it must wait for publication and proceed with the
// protocol-level interrupt instead of returning a hard error that causes the
// caller to tear down the persistent app-server.
func TestAbortWaitsForTurnIDPublication_OB4(t *testing.T) {
	session := startTestSession(t, nil)

	// Simulate abort racing turn-start: set up an active turn with no id.
	turn := &activeTurn{
		done:        make(chan struct{}),
		idPublished: make(chan struct{}),
		tools:       make(map[string]ToolCall),
		results:     make(map[string]bool),
	}
	session.mu.Lock()
	session.active = turn
	session.mu.Unlock()
	t.Cleanup(func() {
		// Ensure the turn is cleared so it doesn't interfere with session.Close.
		session.mu.Lock()
		if session.active == turn {
			session.active = nil
		}
		if !turn.closed {
			turn.closed = true
			close(turn.done)
		}
		session.mu.Unlock()
	})

	// Publish the turn id after a short delay (simulates turn/start response).
	go func() {
		time.Sleep(50 * time.Millisecond)
		session.mu.Lock()
		turn.publishID("turn-late")
		session.mu.Unlock()
	}()

	// Abort must wait for publication, then send turn/interrupt.
	// The fake CLI handles turn/interrupt generically and emits
	// turn/completed(interrupted), which closes turn.done.
	start := time.Now()
	err := session.Abort(context.Background())
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Fatalf("Abort returned after %v — did not wait for turn id publication", elapsed)
	}
	if err != nil {
		t.Fatalf("Abort returned error after waiting for publication: %v", err)
	}
	if !session.Alive() {
		t.Fatal("app-server was killed by abort — should have been preserved")
	}
}

// TestAbortEscalatesWhenTurnIDNeverPublished_OB4 verifies that Abort does
// not block forever if the turn id is never published: it escalates to a
// controlled teardown after CloseTimeout.
func TestAbortEscalatesWhenTurnIDNeverPublished_OB4(t *testing.T) {
	session := startTestSession(t, nil)

	// Set up an active turn with no id and never publish it.
	turn := &activeTurn{
		done:        make(chan struct{}),
		idPublished: make(chan struct{}),
		tools:       make(map[string]ToolCall),
		results:     make(map[string]bool),
	}
	session.mu.Lock()
	session.active = turn
	session.mu.Unlock()

	err := session.Abort(context.Background())
	if err == nil {
		t.Fatal("Abort should have escalated when turn id was never published")
	}
	if !strings.Contains(err.Error(), "turn id not published") {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.Alive() {
		t.Fatal("app-server should have been killed by escalation")
	}
}

func TestRealCodexAppServerTwoTurns(t *testing.T) {
	if os.Getenv("CODEX_REAL_SMOKE") != "1" {
		t.Skip("set CODEX_REAL_SMOKE=1 to use the installed authenticated Codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session, err := Start(ctx, Options{WorkDir: t.TempDir(), Permission: "auto", CloseTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	pid := session.core.PID()
	for _, prompt := range []string{"Reply with exactly: turn-one", "Reply with exactly: turn-two"} {
		if err := session.Send(ctx, Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		events := readCodexTurnWithContext(t, ctx, session.Events())
		if eventOfKind(events, EventFinish) == nil || eventOfKind(events, EventError) != nil {
			t.Fatalf("events = %#v", events)
		}
		if session.core.PID() != pid || !session.Alive() {
			t.Fatalf("app-server changed after turn: pid=%d current=%d alive=%v", pid, session.core.PID(), session.Alive())
		}
	}
}

func startTestSession(t *testing.T, extra map[string]string) *Session {
	t.Helper()
	return startTestSessionWithOptions(t, Options{}, extra)
}

func startTestSessionWithOptions(t *testing.T, options Options, extra map[string]string) *Session {
	t.Helper()
	env := []string{"GO_WANT_CODEX_APP_SERVER_HELPER=1"}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	options.Command = []string{os.Args[0], "-test.run=TestCodexAppServerHelper", "--"}
	options.Env = env
	options.WorkDir = t.TempDir()
	options.CloseTimeout = time.Second
	session, err := Start(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

func readCodexTurn(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return readCodexTurnWithContext(t, ctx, events)
}

func readCodexTurnWithContext(t *testing.T, ctx context.Context, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("events closed early: %#v", got)
			}
			got = append(got, event)
			if event.Kind == EventFinish || event.Kind == EventError {
				return got
			}
		case <-ctx.Done():
			t.Fatalf("turn timed out: %#v", got)
		}
	}
}

func eventOfKind(events []Event, kind EventKind) *Event {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

func eventKindCount(events []Event, kind EventKind) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func textEvent(events []Event, want string) *Event {
	for i := range events {
		if events[i].Kind == EventText && strings.Contains(events[i].Text, want) {
			return &events[i]
		}
	}
	return nil
}

type rpcLogEntry struct {
	PID    int             `json:"pid"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func readRPCLog(t *testing.T, path string) []rpcLogEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []rpcLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry rpcLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func assertMethodCount(t *testing.T, entries []rpcLogEntry, method string, want int) {
	t.Helper()
	got := 0
	for _, entry := range entries {
		if entry.Method == method {
			got++
		}
	}
	if got != want {
		t.Fatalf("method %s count = %d, want %d; entries=%#v", method, got, want, entries)
	}
}

func uniquePIDs(entries []rpcLogEntry) int {
	values := map[int]struct{}{}
	for _, entry := range entries {
		values[entry.PID] = struct{}{}
	}
	return len(values)
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s not created", path)
}

func TestCodexAppServerHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_APP_SERVER_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	turn := 0
	blockedPrompt := ""
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		logHelperRPC(request.Method, request.Params)
		if len(request.ID) == 0 {
			continue
		}
		switch request.Method {
		case "initialize":
			emitRPCResult(request.ID, map[string]any{"userAgent": "test"})
		case "thread/start":
			emitRPCResult(request.ID, map[string]any{"thread": map[string]any{"id": "thread-native-1"}})
		case "thread/resume":
			var params struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			emitRPCResult(request.ID, map[string]any{"thread": map[string]any{"id": params.ThreadID}})
		case "turn/start":
			turn++
			turnID := "turn-" + strconv.Itoa(turn)
			emitRPCResult(request.ID, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})
			var params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(request.Params, &params)
			prompt := ""
			if len(params.Input) > 0 {
				prompt = params.Input[0].Text
			}
			emitRPCNotification("turn/started", map[string]any{"threadId": "thread-native-1", "turn": map[string]any{"id": turnID}})
			if prompt == "block" || prompt == "block-fail" {
				blockedPrompt = prompt
				_ = os.WriteFile(os.Getenv("CODEX_TEST_READY"), []byte("ready"), 0o600)
				continue
			}
			emitTurn(turnID, prompt)
		case "turn/interrupt":
			var params struct {
				TurnID string `json:"turnId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			emitRPCResult(request.ID, map[string]any{})
			status := "interrupted"
			completed := map[string]any{"id": params.TurnID, "status": status}
			if blockedPrompt == "block-fail" {
				// Simulate a native turn that FAILS as a side effect of the
				// interrupt (the D2 abort-convergence scenario).
				status = "failed"
				completed["status"] = status
				completed["error"] = map[string]any{"message": "interrupted mid-tool"}
				blockedPrompt = ""
			}
			emitRPCNotification("turn/completed", map[string]any{"threadId": "thread-native-1", "turn": completed})
		default:
			emitRPCError(request.ID, -32601, "method not found")
		}
	}
	if err := scanner.Err(); err != nil {
		os.Exit(2)
	}
}

func emitTurn(turnID, prompt string) {
	emitRPCNotification("item/started", map[string]any{"threadId": "thread-native-1", "turnId": turnID, "item": map[string]any{"id": "reason-1", "type": "reasoning", "summary": []string{"safe reasoning summary"}}})
	if prompt == "tool" {
		emitRPCNotification("item/started", map[string]any{"threadId": "thread-native-1", "turnId": turnID, "item": map[string]any{"id": "item-tool-1", "type": "commandExecution", "command": "pwd", "status": "inProgress"}})
		emitRPCNotification("item/completed", map[string]any{"threadId": "thread-native-1", "turnId": turnID, "item": map[string]any{"id": "item-tool-1", "type": "commandExecution", "command": "pwd", "status": "completed", "aggregatedOutput": "/tmp", "exitCode": 0}})
		emitRPCNotification("item/completed", map[string]any{"threadId": "thread-native-1", "turnId": turnID, "item": map[string]any{"id": "item-tool-1", "type": "commandExecution", "command": "pwd", "status": "completed", "aggregatedOutput": "/tmp", "exitCode": 0}})
	}
	emitRPCNotification("item/agentMessage/delta", map[string]any{"threadId": "thread-native-1", "turnId": turnID, "itemId": "message-1", "delta": "echo:" + prompt})
	emitRPCNotification("turn/completed", map[string]any{"threadId": "thread-native-1", "turn": map[string]any{"id": turnID, "status": "completed"}, "usage": map[string]any{"inputTokens": 3, "outputTokens": 4, "totalTokens": 7}})
}

func logHelperRPC(method string, params json.RawMessage) {
	path := os.Getenv("CODEX_TEST_LOG")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	defer file.Close()
	data, _ := json.Marshal(rpcLogEntry{PID: os.Getpid(), Method: method, Params: params})
	_, _ = fmt.Fprintln(file, string(data))
}

func emitRPCResult(id json.RawMessage, result any) {
	emitRPC(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func emitRPCError(id json.RawMessage, code int, message string) {
	emitRPC(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func emitRPCNotification(method string, params any) {
	emitRPC(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func emitRPC(value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintln(os.Stdout, string(data))
}

// TestAbortedTurnFailingAfterInterruptEndsCancelled_D2 pins the converged
// abort semantics: when a native turn FAILS as a side effect of the interrupt,
// the turn still ends with finish("cancelled") and the failure is logged, not
// emitted as an error event (which the API would record as RunFailed). Before
// D2 this path emitted EventError without a finish marker.
func TestAbortedTurnFailingAfterInterruptEndsCancelled_D2(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	session := startTestSession(t, map[string]string{"CODEX_TEST_READY": ready})

	if err := session.Send(context.Background(), Input{Prompt: "block-fail"}); err != nil {
		t.Fatal(err)
	}
	waitFile(t, ready)
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := readCodexTurn(t, session.Events())
	finish := eventOfKind(events, EventFinish)
	if finish == nil {
		t.Fatalf("no finish event after aborted turn: %#v", events)
	}
	if finish.FinishReason != "cancelled" {
		t.Fatalf("finish reason = %q, want cancelled (converged abort semantics)", finish.FinishReason)
	}
	if failure := eventOfKind(events, EventError); failure != nil {
		t.Fatalf("abort side-effect failure must be logged, not emitted: %#v", failure)
	}
	if !session.Alive() {
		t.Fatal("app-server was killed by an interrupt that the native side answered")
	}
}
