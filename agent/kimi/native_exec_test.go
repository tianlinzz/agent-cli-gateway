package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildArgsUsesPersistentACP(t *testing.T) {
	if got := strings.Join(BuildArgs(Options{}, "", ""), " "); got != "acp" {
		t.Fatalf("args = %q, want acp", got)
	}
}

func TestSessionUsesOneACPProcessForMultipleTurns(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "rpc.jsonl")
	session := startKimiTestSession(t, Options{}, map[string]string{"KIMI_TEST_LOG": logFile})
	for _, prompt := range []string{"first", "tool"} {
		if err := session.Send(context.Background(), Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		events := readKimiTurn(t, session.Events())
		if reasoning := findKimiEvent(events, EventReasoning); reasoning == nil || reasoning.Reasoning == nil || reasoning.Reasoning.Text != "safe thought summary" {
			t.Fatalf("reasoning summary = %#v", reasoning)
		}
		if findKimiEvent(events, EventFinish) == nil {
			t.Fatalf("events = %#v", events)
		}
	}
	entries := readKimiRPCLog(t, logFile)
	assertKimiMethodCount(t, entries, "initialize", 1)
	assertKimiMethodCount(t, entries, "session/new", 1)
	assertKimiMethodCount(t, entries, "session/resume", 0)
	assertKimiMethodCount(t, entries, "session/prompt", 2)
	if kimiUniquePIDs(entries) != 1 {
		t.Fatalf("RPCs used multiple PIDs: %#v", entries)
	}
}

func TestSessionResumesACPOnce(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "rpc.jsonl")
	session := startKimiTestSession(t, Options{ResumeID: "session-existing"}, map[string]string{"KIMI_TEST_LOG": logFile})
	if err := session.Send(context.Background(), Input{Prompt: "hello"}); err != nil {
		t.Fatal(err)
	}
	readKimiTurn(t, session.Events())
	entries := readKimiRPCLog(t, logFile)
	assertKimiMethodCount(t, entries, "session/new", 0)
	assertKimiMethodCount(t, entries, "session/resume", 1)
	assertKimiMethodCount(t, entries, "session/prompt", 1)
}

func TestACPToolEventsUseStableID(t *testing.T) {
	session := startKimiTestSession(t, Options{}, nil)
	if err := session.Send(context.Background(), Input{Prompt: "tool"}); err != nil {
		t.Fatal(err)
	}
	events := readKimiTurn(t, session.Events())
	use := findKimiEvent(events, EventToolUse)
	result := findKimiEvent(events, EventToolResult)
	if use == nil || result == nil || use.Tool == nil || result.Tool == nil || use.Tool.ID != "tool-1" || result.Tool.ID != use.Tool.ID {
		t.Fatalf("tool events = %#v", events)
	}
}

func TestAbortCancelsPromptAndPreservesACPProcess(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "rpc.jsonl")
	ready := filepath.Join(t.TempDir(), "ready")
	session := startKimiTestSession(t, Options{}, map[string]string{"KIMI_TEST_LOG": logFile, "KIMI_TEST_READY": ready})
	if err := session.Send(context.Background(), Input{Prompt: "block"}); err != nil {
		t.Fatal(err)
	}
	waitKimiFile(t, ready)
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	readKimiTurn(t, session.Events())
	if !session.Alive() {
		t.Fatal("ACP process closed after turn cancellation")
	}
	if err := session.Send(context.Background(), Input{Prompt: "after"}); err != nil {
		t.Fatal(err)
	}
	readKimiTurn(t, session.Events())
	entries := readKimiRPCLog(t, logFile)
	assertKimiMethodCount(t, entries, "session/cancel", 1)
	assertKimiMethodCount(t, entries, "session/prompt", 2)
	if kimiUniquePIDs(entries) != 1 {
		t.Fatalf("abort restarted ACP: %#v", entries)
	}
}

func TestRealKimiACPPersistentProcessTwoTurns(t *testing.T) {
	if os.Getenv("KIMI_REAL_SMOKE") != "1" {
		t.Skip("set KIMI_REAL_SMOKE=1 to use the installed authenticated Kimi Code CLI")
	}
	command := strings.TrimSpace(os.Getenv("KIMI_REAL_COMMAND"))
	if command == "" {
		command = "kimi"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session, err := Start(ctx, Options{
		Command:    []string{command},
		WorkDir:    t.TempDir(),
		Permission: "auto",
		Timeout:    2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())

	pid := session.process.PID()
	nativeID := session.NativeSessionID()
	if nativeID == "" {
		t.Fatal("Kimi ACP returned an empty native session ID")
	}
	for _, prompt := range []string{"Reply with exactly: turn-one", "Reply with exactly: turn-two"} {
		if err := session.Send(ctx, Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		events := readKimiTurnWithContext(t, ctx, session.Events())
		if findKimiEvent(events, EventFinish) == nil || findKimiEvent(events, EventError) != nil {
			t.Fatalf("events = %#v", events)
		}
		if session.process.PID() != pid || session.NativeSessionID() != nativeID || !session.Alive() {
			t.Fatalf("ACP lifecycle changed after turn: pid=%d current=%d native_id=%q current_native_id=%q alive=%v", pid, session.process.PID(), nativeID, session.NativeSessionID(), session.Alive())
		}
	}
}

func startKimiTestSession(t *testing.T, options Options, extra map[string]string) *Session {
	t.Helper()
	env := []string{"GO_WANT_KIMI_ACP_HELPER=1"}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	options.Command = []string{os.Args[0], "-test.run=TestKimiACPHelper", "--"}
	options.Env = env
	options.WorkDir = t.TempDir()
	options.Timeout = time.Second
	session, err := Start(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

func readKimiTurn(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return readKimiTurnWithContext(t, ctx, events)
}

func readKimiTurnWithContext(t *testing.T, ctx context.Context, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("events closed: %#v", got)
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

func findKimiEvent(events []Event, kind EventKind) *Event {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

type kimiRPCLog struct {
	PID    int             `json:"pid"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func readKimiRPCLog(t *testing.T, path string) []kimiRPCLog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []kimiRPCLog
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry kimiRPCLog
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func assertKimiMethodCount(t *testing.T, entries []kimiRPCLog, method string, want int) {
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

func kimiUniquePIDs(entries []kimiRPCLog) int {
	values := map[int]struct{}{}
	for _, entry := range entries {
		values[entry.PID] = struct{}{}
	}
	return len(values)
}

func waitKimiFile(t *testing.T, path string) {
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

var kimiHelperWriteMu sync.Mutex

func TestKimiACPHelper(t *testing.T) {
	if os.Getenv("GO_WANT_KIMI_ACP_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	turn := 0
	var blockedID json.RawMessage
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		logKimiRPC(request.Method, request.Params)
		switch request.Method {
		case "initialize":
			emitKimiResult(request.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
		case "session/new":
			emitKimiResult(request.ID, map[string]any{"sessionId": "session-native-1"})
		case "session/resume":
			var params struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			emitKimiResult(request.ID, map[string]any{"sessionId": params.SessionID})
		case "session/prompt":
			turn++
			var params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(request.Params, &params)
			prompt := ""
			if len(params.Prompt) > 0 {
				prompt = params.Prompt[0].Text
			}
			if prompt == "block" {
				blockedID = append(json.RawMessage(nil), request.ID...)
				_ = os.WriteFile(os.Getenv("KIMI_TEST_READY"), []byte("ready"), 0o600)
				continue
			}
			go emitKimiTurn(request.ID, turn, prompt, "end_turn")
		case "session/cancel":
			if len(blockedID) > 0 {
				emitKimiResult(blockedID, map[string]any{"stopReason": "cancelled"})
				blockedID = nil
			}
		default:
			if len(request.ID) > 0 {
				emitKimiError(request.ID, -32601, "method not found")
			}
		}
	}
}

func emitKimiTurn(id json.RawMessage, turn int, prompt, stop string) {
	emitKimiNotification("session/update", map[string]any{"sessionId": "session-native-1", "update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "safe thought summary"}}})
	if prompt == "tool" {
		emitKimiNotification("session/update", map[string]any{"sessionId": "session-native-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool-1", "title": "Shell", "rawInput": map[string]any{"command": "pwd"}, "status": "in_progress"}})
		emitKimiNotification("session/update", map[string]any{"sessionId": "session-native-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tool-1", "status": "completed", "content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "/tmp"}}}}})
	}
	emitKimiNotification("session/update", map[string]any{"sessionId": "session-native-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "echo:" + prompt}}})
	emitKimiNotification("session/update", map[string]any{"sessionId": "session-native-1", "update": map[string]any{"sessionUpdate": "usage_update", "used": 5, "size": 8192}})
	emitKimiResult(id, map[string]any{"stopReason": stop, "turn": strconv.Itoa(turn)})
}

func logKimiRPC(method string, params json.RawMessage) {
	path := os.Getenv("KIMI_TEST_LOG")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	defer file.Close()
	data, _ := json.Marshal(kimiRPCLog{PID: os.Getpid(), Method: method, Params: params})
	_, _ = fmt.Fprintln(file, string(data))
}

func emitKimiResult(id json.RawMessage, result any) {
	emitKimi(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func emitKimiError(id json.RawMessage, code int, message string) {
	emitKimi(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func emitKimiNotification(method string, params any) {
	emitKimi(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func emitKimi(value any) {
	data, _ := json.Marshal(value)
	kimiHelperWriteMu.Lock()
	defer kimiHelperWriteMu.Unlock()
	_, _ = fmt.Fprintln(os.Stdout, string(data))
}
