package codex

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

func TestBuildArgsFreshAndResume(t *testing.T) {
	opts := NormalizeOptions(Options{WorkDir: "/workspace", Model: "gpt-5", ReasoningEffort: "high"})
	fresh := BuildArgs(opts, "")
	if !hasSequence(fresh, "exec", "--skip-git-repo-check", "--sandbox", "danger-full-access") || !hasSequence(fresh, "--json", "--cd", "/workspace", "-") {
		t.Fatalf("fresh args = %v", fresh)
	}
	resume := BuildArgs(opts, "thread-1")
	if !hasSequence(resume, "exec", "resume", "--skip-git-repo-check") || !hasSequence(resume, "-c", `sandbox_mode="danger-full-access"`) || !hasSequence(resume, "thread-1", "--json", "-") {
		t.Fatalf("resume args = %v", resume)
	}
	for _, arg := range resume {
		if arg == "--cd" || arg == "--sandbox" {
			t.Fatalf("resume args contain unsupported %q: %v", arg, resume)
		}
	}
}

func TestSessionStartsPerTurnAndResumesNativeThread(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "invocations.jsonl")
	session := newTestSession(t, map[string]string{"CODEX_TEST_LOG": logFile})

	for _, prompt := range []string{"first\nline", "second"} {
		if err := session.Send(context.Background(), Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		events := readCodexTurn(t, session.Events())
		text := eventOfKind(events, EventText)
		if text == nil || text.Text != "echo:"+prompt {
			t.Fatalf("events = %#v", events)
		}
		usage := eventOfKind(events, EventUsage)
		if usage == nil || usage.Usage.TotalTokens != 7 {
			t.Fatalf("usage = %#v", usage)
		}
	}

	entries := readInvocationLog(t, logFile)
	if len(entries) != 2 {
		t.Fatalf("invocations = %d, want 2", len(entries))
	}
	if entries[0].Stdin != "first\nline" || entries[1].Stdin != "second" {
		t.Fatalf("stdin values = %#v", entries)
	}
	if contains(entries[0].Args, "resume") || !contains(entries[1].Args, "resume") || !contains(entries[1].Args, "thread-native-1") {
		t.Fatalf("fresh/resume args = %#v", entries)
	}
}

func TestToolCommentaryPrecedesToolEvents(t *testing.T) {
	session := newTestSession(t, nil)
	if err := session.Send(context.Background(), Input{Prompt: "tool"}); err != nil {
		t.Fatal(err)
	}
	events := readCodexTurn(t, session.Events())
	want := []EventKind{EventNativeSession, EventText, EventToolUse, EventToolResult, EventFinish, EventUsage}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for i := range want {
		if events[i].Kind != want[i] {
			t.Fatalf("event %d = %q, want %q (%#v)", i, events[i].Kind, want[i], events)
		}
	}
}

func TestAbortKillsCurrentTurnButSessionCanResume(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	session := newTestSession(t, map[string]string{"CODEX_TEST_READY": readyFile})
	if err := session.Send(context.Background(), Input{Prompt: "block"}); err != nil {
		t.Fatal(err)
	}
	waitFile(t, readyFile)
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !session.Alive() {
		t.Fatal("resume-per-turn session was closed by Abort")
	}
	if err := session.Send(context.Background(), Input{Prompt: "after"}); err != nil {
		t.Fatal(err)
	}
	if events := readCodexTurn(t, session.Events()); eventOfKind(events, EventFinish) == nil {
		t.Fatalf("post-abort events = %#v", events)
	}
}

func TestReadRolloutUsageUsesLastTokenCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-thread.jsonl")
	data := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":4,"output_tokens":5,"total_tokens":9}}}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := ReadRolloutUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.InputTokens != 4 || usage.OutputTokens != 5 || usage.TotalTokens != 9 {
		t.Fatalf("usage = %#v", usage)
	}
}

func newTestSession(t *testing.T, extra map[string]string) *Session {
	t.Helper()
	env := []string{"GO_WANT_CODEX_NATIVE_HELPER=1"}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	session := New(Options{
		Command: []string{os.Args[0], "-test.run=TestCodexNativeHelper", "--", "exec"},
		Env:     env, WorkDir: t.TempDir(), CloseTimeout: time.Second,
	})
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

func readCodexTurn(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("events closed early: %#v", got)
			}
			got = append(got, event)
			if event.Kind == EventUsage {
				return got
			}
		case <-deadline:
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

type invocation struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func readInvocationLog(t *testing.T, path string) []invocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var values []invocation
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var value invocation
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	return values
}

func hasSequence(values []string, sequence ...string) bool {
	for i := 0; i+len(sequence) <= len(values); i++ {
		match := true
		for j := range sequence {
			if values[i+j] != sequence[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

func TestCodexNativeHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_NATIVE_HELPER") != "1" {
		return
	}
	input, err := bufio.NewReader(os.Stdin).ReadString(0)
	if err != nil && len(input) == 0 {
		inputBytes, readErr := os.ReadFile("/dev/stdin")
		if readErr == nil {
			input = string(inputBytes)
		}
	}
	input = strings.TrimSpace(input)
	if logPath := os.Getenv("CODEX_TEST_LOG"); logPath != "" {
		file, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr != nil {
			os.Exit(2)
		}
		encoded, _ := json.Marshal(invocation{Args: os.Args, Stdin: input})
		fmt.Fprintln(file, string(encoded))
		file.Close()
	}
	emitCodex(map[string]any{"type": "thread.started", "thread_id": "thread-native-1"})
	emitCodex(map[string]any{"type": "turn.started"})
	if input == "block" {
		if err := os.WriteFile(os.Getenv("CODEX_TEST_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		select {}
	}
	if input == "tool" {
		emitCodex(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "output_text", "text": "working"}}}})
		emitCodex(map[string]any{"type": "item.started", "item": map[string]any{"type": "command_execution", "command": "pwd"}})
		emitCodex(map[string]any{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "pwd", "status": "completed", "exit_code": float64(0), "aggregated_output": "/tmp"}})
	} else {
		emitCodex(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "output_text", "text": "echo:" + input}}}})
	}
	emitCodex(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 3, "output_tokens": 4, "total_tokens": 7}})
}

func emitCodex(value any) {
	data, _ := json.Marshal(value)
	fmt.Println(string(data))
}
