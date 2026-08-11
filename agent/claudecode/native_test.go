package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildArgsDisablesAskUserQuestionExactlyOnce(t *testing.T) {
	opts := NormalizeOptions(Options{
		Model:           "haiku",
		Mode:            "accept-edits",
		DisallowedTools: []string{"Bash", "AskUserQuestion"},
	})
	args := BuildArgs(opts, "")
	joined := strings.Join(args, " ")
	if strings.Count(joined, "AskUserQuestion") != 1 {
		t.Fatalf("args contain AskUserQuestion %d times: %v", strings.Count(joined, "AskUserQuestion"), args)
	}
	if !containsSequence(args, "--model", "haiku") || !containsSequence(args, "--permission-mode", "acceptEdits") {
		t.Fatalf("normalized args missing model or mode: %v", args)
	}
}

func TestThinkingBlocksDoNotLeakIntoAssistantText(t *testing.T) {
	session := &Session{events: make(chan Event, 4), ctx: context.Background()}
	session.handleAssistant(map[string]any{"message": map[string]any{"content": []any{
		map[string]any{"type": "thinking", "thinking": "private chain of thought"},
		map[string]any{"type": "text", "text": "OK"},
	}}})

	select {
	case event := <-session.events:
		if event.Kind != EventText || event.Text != "OK" {
			t.Fatalf("public event = %#v, want final text only", event)
		}
	default:
		t.Fatal("missing final text event")
	}
	select {
	case event := <-session.events:
		t.Fatalf("unexpected leaked event: %#v", event)
	default:
	}
}

func TestSessionUsesOnePersistentProcessForMultipleTurns(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	session := startTestSession(t, "normal", map[string]string{"CLAUDE_TEST_PID_FILE": pidFile})

	for _, prompt := range []string{"first", "second"} {
		if err := session.Send(context.Background(), Input{Prompt: prompt}); err != nil {
			t.Fatalf("send %q: %v", prompt, err)
		}
		events := readTurn(t, session.Events())
		text := findEvent(events, EventText)
		if text == nil || text.Text != "echo:"+prompt {
			t.Fatalf("events for %q = %#v", prompt, events)
		}
		if events[len(events)-1].Kind != EventUsage || events[len(events)-1].Usage.TotalTokens != 12 {
			t.Fatalf("usage for %q = %#v", prompt, events[len(events)-1])
		}
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Fields(string(data)); len(lines) != 1 {
		t.Fatalf("native process starts = %d, want 1 (%q)", len(lines), data)
	}
	if got := session.NativeSessionID(); got != "native-claude-session" {
		t.Fatalf("native session id = %q", got)
	}
}

func TestRealClaudePersistentProcessTwoTurns(t *testing.T) {
	if os.Getenv("CLAUDE_REAL_SMOKE") != "1" {
		t.Skip("set CLAUDE_REAL_SMOKE=1 to use the installed authenticated Claude CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session, err := Start(ctx, Options{WorkDir: t.TempDir(), Permission: "auto", CloseTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	pid := session.process.PID()
	for _, prompt := range []string{"Reply with exactly: turn-one", "Reply with exactly: turn-two"} {
		if err := session.Send(ctx, Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		var events []Event
		for {
			select {
			case event, ok := <-session.Events():
				if !ok {
					t.Fatalf("Claude exited during turn: %#v", events)
				}
				events = append(events, event)
				if event.Kind == EventError {
					t.Fatalf("Claude turn failed: %#v", events)
				}
				if event.Kind == EventUsage {
					goto settled
				}
			case <-ctx.Done():
				t.Fatalf("Claude turn timed out: %#v", events)
			}
		}
	settled:
		if session.process.PID() != pid || !session.Alive() {
			t.Fatalf("Claude process changed after turn: pid=%d current=%d alive=%v", pid, session.process.PID(), session.Alive())
		}
	}
}

func TestAskUserQuestionIsDeniedWithoutHanging(t *testing.T) {
	responseFile := filepath.Join(t.TempDir(), "response.json")
	session := startTestSession(t, "ask", map[string]string{"CLAUDE_TEST_RESPONSE_FILE": responseFile})
	if err := session.Send(context.Background(), Input{Prompt: "ask"}); err != nil {
		t.Fatal(err)
	}
	events := readTurn(t, session.Events())
	errorEvent := findEvent(events, EventError)
	if errorEvent == nil || errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "AskUserQuestion") {
		t.Fatalf("events = %#v, want AskUserQuestion error then finish", events)
	}

	data, err := os.ReadFile(responseFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"behavior":"deny"`) {
		t.Fatalf("permission response = %s, want deny", data)
	}
}

func TestAbortTerminatesUncancellableExecution(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	session := startTestSession(t, "block", map[string]string{"CLAUDE_TEST_READY_FILE": readyFile})
	if err := session.Send(context.Background(), Input{Prompt: "block"}); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyFile)
	if err := session.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if session.Alive() {
		t.Fatal("session remains alive after aborting an uncancellable turn")
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-session.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("event channel did not close after abort")
		}
	}
}

func startTestSession(t *testing.T, mode string, extra map[string]string) *Session {
	t.Helper()
	env := []string{"GO_WANT_CLAUDE_NATIVE_HELPER=1"}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	session, err := Start(context.Background(), Options{
		Command:      []string{os.Args[0], "-test.run=TestClaudeNativeHelper", "--", mode},
		Env:          env,
		WorkDir:      t.TempDir(),
		Permission:   "auto",
		CloseTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})
	return session
}

func readTurn(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed before finish: %#v", got)
			}
			got = append(got, event)
			if event.Kind == EventUsage || (event.Kind == EventFinish && event.FinishReason != "end_turn") {
				return got
			}
			if event.Kind == EventFinish {
				// The native protocol emits usage immediately after finish. Keep
				// reading so ordering is covered.
				continue
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for turn: %#v", got)
		}
	}
}

func findEvent(events []Event, kind EventKind) *Event {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

func containsSequence(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		matched := true
		for j := range want {
			if args[i+j] != want[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s was not created", path)
}

func TestClaudeNativeHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CLAUDE_NATIVE_HELPER") != "1" {
		return
	}
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if mode == "" {
		os.Exit(2)
	}
	if path := os.Getenv("CLAUDE_TEST_PID_FILE"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(file, os.Getpid())
		file.Close()
	}
	emitClaudeJSON(map[string]any{"type": "system", "model": "test", "session_id": "native-claude-session"})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var input struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &input); err != nil {
			os.Exit(2)
		}
		switch mode {
		case "normal":
			emitClaudeJSON(map[string]any{"type": "assistant", "message": map[string]any{
				"usage":   map[string]any{"input_tokens": 10},
				"content": []any{map[string]any{"type": "text", "text": "echo:" + input.Message.Content}},
			}})
			emitClaudeJSON(map[string]any{"type": "result", "session_id": "native-claude-session", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}})
		case "ask":
			emitClaudeJSON(map[string]any{"type": "control_request", "request_id": "ask-1", "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": map[string]any{"question": "continue?"},
			}})
			if !scanner.Scan() {
				os.Exit(3)
			}
			if err := os.WriteFile(os.Getenv("CLAUDE_TEST_RESPONSE_FILE"), append([]byte(nil), scanner.Bytes()...), 0o600); err != nil {
				os.Exit(2)
			}
			emitClaudeJSON(map[string]any{"type": "result", "session_id": "native-claude-session", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
			return
		case "block":
			if err := os.WriteFile(os.Getenv("CLAUDE_TEST_READY_FILE"), []byte("ready"), 0o600); err != nil {
				os.Exit(2)
			}
			select {}
		}
	}
}

func emitClaudeJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		os.Exit(2)
	}
	fmt.Println(string(data))
}
