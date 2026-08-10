// Claude Code adapter tests — migrated from the upstream in-repo
// agent/claudecode package (originally cc-connect) and adapted to the new
// runtime contract.
//
// These tests cover CLI arg building, stream-json event mapping (system,
// assistant, user/tool_result, result/usage, compaction), permission
// requests, abort, abnormal exit and session lifecycle. They never require a
// real Claude login: every subprocess test drives a fake `claude` shell
// script whose stdout is the canonical stream-json line format. IM/provider/
// management-specific tests were not migrated.
package claudecode

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gwrt "github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestNormalizePermissionMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"dontAsk", "dontAsk"},
		{"dontask", "dontAsk"},
		{"dont-ask", "dontAsk"},
		{"dont_ask", "dontAsk"},
		{"auto", "auto"},
		{"bypassPermissions", "bypassPermissions"},
		{"yolo", "bypassPermissions"},
		{"acceptEdits", "acceptEdits"},
		{"edit", "acceptEdits"},
		{"plan", "plan"},
		{"", "default"},
		{"unknown", "default"},
	}
	for _, tt := range tests {
		if got := normalizePermissionMode(tt.input); got != tt.want {
			t.Errorf("normalizePermissionMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeEffort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"low", "low"}, {"medium", "medium"}, {"med", "medium"},
		{"high", "high"}, {"max", "max"}, {"xhigh", ""},
	} {
		if got := normalizeEffort(tc.in); got != tc.want {
			t.Fatalf("normalizeEffort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsClaudeEditTool(t *testing.T) {
	for _, tool := range []string{"Edit", "Write", "NotebookEdit", "MultiEdit"} {
		if !isClaudeEditTool(tool) {
			t.Fatalf("isClaudeEditTool(%q) = false, want true", tool)
		}
	}
	if isClaudeEditTool("Bash") {
		t.Fatal("isClaudeEditTool(Bash) = true, want false")
	}
}

func TestSummarizeInput(t *testing.T) {
	if got := summarizeInput("Bash", map[string]any{"command": "ls -la"}); got != "ls -la" {
		t.Fatalf("summarizeInput(Bash) = %q, want ls -la", got)
	}
	if got := summarizeInput("Read", map[string]any{"file_path": "src/main.go"}); got != "src/main.go" {
		t.Fatalf("summarizeInput(Read) = %q, want src/main.go", got)
	}
	if got := summarizeInput("Grep", map[string]any{"pattern": "TODO"}); got != "TODO" {
		t.Fatalf("summarizeInput(Grep) = %q, want TODO", got)
	}
	if got := summarizeInput("UnknownTool", map[string]any{"a": "b"}); got != `{"a":"b"}` {
		t.Fatalf("summarizeInput(UnknownTool) = %q, want JSON fallback", got)
	}
}

func TestIsCompactionResult(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{"nil_subtype", map[string]any{"type": "result"}, false},
		{"empty_subtype", map[string]any{"type": "result", "subtype": ""}, false},
		{"success_subtype", map[string]any{"type": "result", "subtype": "success"}, false},
		{"compact_subtype", map[string]any{"type": "result", "subtype": "compact"}, true},
		{"compaction_subtype", map[string]any{"type": "result", "subtype": "compaction"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCompactionResult(tc.raw); got != tc.want {
				t.Errorf("isCompactionResult(%v) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseClaudeUsage(t *testing.T) {
	in, out, cc, cr := parseClaudeUsage(map[string]any{
		"input_tokens":                float64(150000),
		"output_tokens":               float64(2000),
		"cache_creation_input_tokens": float64(100),
		"cache_read_input_tokens":     float64(500),
	})
	if in != 150000 || out != 2000 || cc != 100 || cr != 500 {
		t.Fatalf("parseClaudeUsage = %d/%d/%d/%d, want 150000/2000/100/500", in, out, cc, cr)
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "CLAUDECODE=1", "HOME=/root", "CLAUDECODE_EXTRA=2"}
	got := filterEnv(env, "CLAUDECODE")
	if len(got) != 3 {
		t.Fatalf("filterEnv = %v, want 3 entries (only the exact CLAUDECODE= key removed)", got)
	}
	for _, e := range got {
		if strings.HasPrefix(e, "CLAUDECODE=") {
			t.Fatalf("filterEnv leaked %q", e)
		}
	}
}

// TestBuildClaudeArgs_Fresh covers the fresh-launch arg surface: stream-json
// IO, permission-mode flag, model, effort and max-context-tokens.
func TestBuildClaudeArgs_Fresh(t *testing.T) {
	args := buildClaudeArgs(claudeLaunchParams{
		model:            "claude-opus-4-7",
		effort:           "high",
		mode:             "acceptEdits",
		allowedTools:     []string{"Edit", "Read"},
		maxContextTokens: 200000,
	})
	want := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--replay-user-messages",
		"--verbose",
		"--permission-mode", "acceptEdits",
		"--allowedTools", "Edit,Read",
		"--effort", "high",
		"--max-context-tokens", "200000",
		"--model", "claude-opus-4-7",
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestNewAdapter_DisablesInteractiveAskUserQuestion(t *testing.T) {
	a, err := NewAdapter(Options{})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if !containsString(a.opts.DisallowedTools, "AskUserQuestion") {
		t.Fatalf("disallowed tools = %v, want AskUserQuestion", a.opts.DisallowedTools)
	}
}

func TestNewAdapter_PreservesConfiguredDisallowedTools(t *testing.T) {
	a, err := NewAdapter(Options{DisallowedTools: []string{"Bash", "Edit"}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	for _, tool := range []string{"Bash", "Edit", "AskUserQuestion"} {
		if !containsString(a.opts.DisallowedTools, tool) {
			t.Fatalf("disallowed tools = %v, want %s", a.opts.DisallowedTools, tool)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestBuildClaudeArgs_Resume passes a native session id — --resume must be
// emitted and no default mode flag added.
func TestBuildClaudeArgs_Resume(t *testing.T) {
	args := buildClaudeArgs(claudeLaunchParams{
		mode:      "default",
		sessionID: "sess-native",
	})
	if !containsSequence(args, []string{"--resume", "sess-native"}) {
		t.Fatalf("args missing --resume sess-native; args=%v", args)
	}
	for _, a := range args {
		if a == "--permission-mode" {
			t.Fatalf("args should not contain --permission-mode for default mode; args=%v", args)
		}
	}
}

func TestBuildClaudeArgs_SystemPromptAndAppendFile(t *testing.T) {
	args := buildClaudeArgs(claudeLaunchParams{
		mode:             "plan",
		systemPrompt:     "You are a gateway agent.",
		appendPromptFile: "/tmp/agent-prompts/append.md",
	})
	if !containsSequence(args, []string{"--system-prompt", "You are a gateway agent."}) {
		t.Fatalf("args missing --system-prompt; args=%v", args)
	}
	if !containsSequence(args, []string{"--append-system-prompt-file", "/tmp/agent-prompts/append.md"}) {
		t.Fatalf("args missing --append-system-prompt-file; args=%v", args)
	}
	if !containsSequence(args, []string{"--permission-mode", "plan"}) {
		t.Fatalf("args missing --permission-mode plan; args=%v", args)
	}
}

func TestBuildClaudeArgs_PluginDirs(t *testing.T) {
	args := buildClaudeArgs(claudeLaunchParams{
		mode:       "default",
		pluginDirs: []string{"/p1", "/p2"},
	})
	if !containsSequence(args, []string{"--plugin-dir", "/p1", "--plugin-dir", "/p2"}) {
		t.Fatalf("args missing plugin dirs; args=%v", args)
	}
}

// TestHandleResultEmitsFinishAndUsage verifies the result event maps to
// EventFinish + EventUsage (input/output/input+output totals).
func TestHandleResultEmitsFinishAndUsage(t *testing.T) {
	cs := newTestClaudeSession(t)
	cs.sessionID.Store("test-session")
	cs.alive.Store(true)

	cs.handleResult(map[string]any{
		"type":       "result",
		"result":     "done",
		"session_id": "test-session",
		"usage": map[string]any{
			"input_tokens":  float64(150000),
			"output_tokens": float64(2000),
		},
	})

	var got []gwrt.Event
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case evt, ok := <-cs.Events():
			if !ok {
				t.Fatalf("events closed after %d events", len(got))
			}
			got = append(got, evt)
		case <-timeout:
			t.Fatalf("timed out; got %+v", got)
		}
	}
	if got[0].Type != gwrt.EventFinish || got[0].FinishReason != "end_turn" {
		t.Fatalf("got[0] = %+v, want finish(end_turn)", got[0])
	}
	if got[1].Type != gwrt.EventUsage || got[1].Usage == nil {
		t.Fatalf("got[1] = %+v, want usage event", got[1])
	}
	if got[1].Usage.InputTokens != 150000 || got[1].Usage.OutputTokens != 2000 || got[1].Usage.TotalTokens != 152000 {
		t.Fatalf("usage = %+v, want 150000/2000/152000", got[1].Usage)
	}
}

// TestHandleResultCompactionIsNotTerminal is the regression test for issue
// #481: Claude Code's mid-turn context compaction emits a `type:"result"`
// event with subtype "compact"/"compaction". The turn must keep running, so
// no EventFinish may be emitted.
func TestHandleResultCompactionIsNotTerminal(t *testing.T) {
	for _, subtype := range []string{"compact", "compaction"} {
		t.Run(subtype, func(t *testing.T) {
			cs := newTestClaudeSession(t)
			cs.sessionID.Store("test-session")
			cs.alive.Store(true)

			cs.handleResult(map[string]any{
				"type":       "result",
				"subtype":    subtype,
				"isCompact":  true,
				"session_id": "test-session",
			})

			select {
			case evt := <-cs.Events():
				t.Fatalf("compaction result emitted event %+v, want none (turn continues)", evt)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestHandleResultNoUsage(t *testing.T) {
	cs := newTestClaudeSession(t)
	cs.sessionID.Store("test-session")
	cs.alive.Store(true)

	cs.handleResult(map[string]any{
		"type":   "result",
		"result": "done",
	})

	select {
	case evt := <-cs.Events():
		if evt.Type != gwrt.EventFinish {
			t.Fatalf("event = %+v, want finish", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for finish event")
	}
	// No usage event may follow.
	select {
	case evt := <-cs.Events():
		t.Fatalf("unexpected usage event: %+v", evt)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleAssistantMapsEvents feeds an assistant message with a tool_use,
// thinking block and text block, asserting the canonical events produced.
func TestHandleAssistantMapsEvents(t *testing.T) {
	cs := newTestClaudeSession(t)
	cs.alive.Store(true)

	cs.handleAssistant(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "let me think"},
				map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls"}},
				map[string]any{"type": "text", "text": "running it"},
			},
		},
	})

	var got []gwrt.Event
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case evt, ok := <-cs.Events():
			if !ok {
				t.Fatalf("events closed after %d events", len(got))
			}
			got = append(got, evt)
		case <-timeout:
			t.Fatalf("timed out; got %+v", got)
		}
	}
	if got[0].Type != gwrt.EventText || got[0].Text != "let me think" {
		t.Fatalf("got[0] = %+v, want thinking text", got[0])
	}
	if got[1].Type != gwrt.EventToolUse || got[1].Tool == nil || got[1].Tool.Name != "Bash" {
		t.Fatalf("got[1] = %+v, want Bash tool use", got[1])
	}
	if cmd, _ := got[1].Tool.Arguments["command"].(string); cmd != "ls" {
		t.Fatalf("tool arguments = %v, want command ls", got[1].Tool.Arguments)
	}
	if got[2].Type != gwrt.EventText || got[2].Text != "running it" {
		t.Fatalf("got[2] = %+v, want text", got[2])
	}
}

// TestHandleAssistantSkipsAskUserQuestion pins the AskUserQuestion skip: the
// tool is surfaced through the permission path, never as a plain tool use.
func TestHandleAssistantSkipsAskUserQuestion(t *testing.T) {
	cs := newTestClaudeSession(t)
	cs.alive.Store(true)

	cs.handleAssistant(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_use", "name": "AskUserQuestion", "input": map[string]any{"question": "pick one"}},
			},
		},
	})

	select {
	case evt := <-cs.Events():
		t.Fatalf("AskUserQuestion emitted %+v, want no event", evt)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleUserEmitsToolResult is the regression test for the bug where
// handleUser silently dropped tool_result content blocks instead of emitting
// EventToolResult. Cases: string content, array content, error result.
func TestHandleUserEmitsToolResult(t *testing.T) {
	cases := []struct {
		name       string
		raw        map[string]any
		wantResult string
		wantError  bool
	}{
		{
			name: "string content",
			raw: map[string]any{
				"type": "user",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "toolu_abc", "is_error": false, "content": "command output here"},
					},
				},
			},
			wantResult: "command output here",
		},
		{
			name: "array content",
			raw: map[string]any{
				"type": "user",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "toolu_def", "is_error": false, "content": []any{
							map[string]any{"type": "text", "text": "line one"},
							map[string]any{"type": "text", "text": "line two"},
						}},
					},
				},
			},
			wantResult: "line one\nline two",
		},
		{
			name: "error result",
			raw: map[string]any{
				"type": "user",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "toolu_err", "is_error": true, "content": "boom"},
					},
				},
			},
			wantResult: "boom",
			wantError:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newTestClaudeSession(t)
			cs.alive.Store(true)

			cs.handleUser(tc.raw)

			select {
			case evt := <-cs.Events():
				if evt.Type != gwrt.EventToolResult || evt.Tool == nil {
					t.Fatalf("event = %+v, want tool result", evt)
				}
				if evt.Tool.Result != tc.wantResult {
					t.Errorf("result = %q, want %q", evt.Tool.Result, tc.wantResult)
				}
				if evt.Tool.IsError != tc.wantError {
					t.Errorf("IsError = %v, want %v", evt.Tool.IsError, tc.wantError)
				}
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for EventToolResult — handleUser dropped the tool_result")
			}
		})
	}
}

// TestPermissionEvent_ControlRequest feeds a can_use_tool control_request
// through handleControlRequest and asserts the canonical EventPermission is
// emitted (the event surface that survives even when the deployment
// auto-approves) and that a control_response is written to stdin.
func TestPermissionEvent_ControlRequest(t *testing.T) {
	cs := newTestClaudeSession(t)
	cs.alive.Store(true)
	cs.setPermissionMode("default")

	// Wire stdin so the response write can be captured.
	stdin := &captureStdin{}
	cs.stdin = stdin

	cs.handleControlRequest(map[string]any{
		"type":       "control_request",
		"request_id": "req-42",
		"request": map[string]any{
			"subtype":   "can_use_tool",
			"tool_name": "Bash",
			"input":     map[string]any{"command": "rm -rf build"},
		},
	})

	select {
	case evt := <-cs.Events():
		if evt.Type != gwrt.EventPermission || evt.Permission == nil {
			t.Fatalf("event = %+v, want permission", evt)
		}
		if evt.Permission.ID != "req-42" || evt.Permission.Action != "Bash" {
			t.Fatalf("permission = %+v, want req-42/Bash", evt.Permission)
		}
		if !strings.Contains(evt.Permission.Detail, "rm -rf build") {
			t.Fatalf("permission detail = %q, want to mention the command", evt.Permission.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission event")
	}

	// The phase-1 auto-approve must write a control_response allow to stdin.
	raw := stdin.String()
	if !strings.Contains(raw, `"behavior":"allow"`) {
		t.Fatalf("control_response = %q, want allow", raw)
	}
	if !strings.Contains(raw, "req-42") {
		t.Fatalf("control_response = %q, want request_id req-42", raw)
	}
}

// TestPermissionEvent_ControlRequestAutoDeny verifies the deny path: with the
// deployment permission set to "deny", the control_response carries deny.
func TestPermissionEvent_ControlRequestAutoDeny(t *testing.T) {
	cs := newTestClaudeSession(t)
	cs.alive.Store(true)
	cs.setPermissionMode("default")
	cs.permission = "deny"

	stdin := &captureStdin{}
	cs.stdin = stdin

	cs.handleControlRequest(map[string]any{
		"type":       "control_request",
		"request_id": "req-7",
		"request": map[string]any{
			"subtype":   "can_use_tool",
			"tool_name": "Bash",
			"input":     map[string]any{"command": "ls"},
		},
	})

	// The permission event must still be emitted (event surface preserved).
	select {
	case evt := <-cs.Events():
		if evt.Type != gwrt.EventPermission {
			t.Fatalf("event = %+v, want permission", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission event")
	}

	raw := stdin.String()
	if !strings.Contains(raw, `"behavior":"deny"`) {
		t.Fatalf("control_response = %q, want deny", raw)
	}
}

// captureStdin is a minimal io.WriteCloser that records what was written, so
// tests can assert the control_response bytes without a blocking pipe.
type captureStdin struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (c *captureStdin) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Write(p)
}

func (c *captureStdin) Close() error { return nil }

func (c *captureStdin) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.String()
}

func TestReadLoop_ChildHoldsStdoutPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(pw, `{"type":"system","session_id":"test-pipe"}`+"\n")
		writeDone <- err
	}()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	cs := &claudeSession{
		cmd:    cmd,
		events: make(chan gwrt.Event, 64),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	cs.alive.Store(true)
	go cs.readLoop(pr, &stderrBuf)

	timeout := time.After(5 * time.Second)
	for {
		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatal(err)
			}
			writeDone = nil
		case _, ok := <-cs.events:
			if !ok {
				if cs.CurrentSessionID() != "test-pipe" {
					t.Fatal("system event session id was lost")
				}
				return
			}
		case <-timeout:
			t.Fatal("HANG: events not closed within 5s - readLoop stuck in scanner.Scan()")
		}
	}
}

func TestReadLoop_CtxCancelClosesChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	cmd := helperCommand(ctx, "err-then-sleep")
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	cs := &claudeSession{
		cmd:    cmd,
		events: make(chan gwrt.Event, 64),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	cs.alive.Store(true)
	go cs.readLoop(pr, &stderrBuf)

	time.Sleep(200 * time.Millisecond)
	cancel()

	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-cs.events:
			if !ok {
				goto closed
			}
		case <-timeout:
			t.Fatal("HANG: events not closed within 5s after ctx cancel")
		}
	}
closed:
	select {
	case <-cs.done:
	case <-timeout:
		t.Fatal("HANG: done not closed within 5s after ctx cancel")
	}
}

func TestClaudeSessionClose_IdempotentNoPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := helperCommand(ctx, "stdin-eof-exit")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	cs := &claudeSession{
		cmd:                 cmd,
		stdin:               stdin,
		ctx:                 ctx,
		cancel:              cancel,
		done:                done,
		gracefulStopTimeout: 200 * time.Millisecond,
	}
	cs.alive.Store(true)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Close panicked: %v", r)
		}
	}()

	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestWriteTempAppendPromptFile_UniquePerCall(t *testing.T) {
	dir := t.TempDir()
	a, err := writeTempAppendPromptFile(dir, "session A")
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	b, err := writeTempAppendPromptFile(dir, "session B")
	if err != nil {
		t.Fatalf("write B: %v", err)
	}
	if a == b {
		t.Fatalf("two writeTempAppendPromptFile calls returned the same path %q", a)
	}
	gotA, _ := os.ReadFile(a)
	gotB, _ := os.ReadFile(b)
	if string(gotA) != "session A" || string(gotB) != "session B" {
		t.Errorf("cross-talk: A=%q B=%q", string(gotA), string(gotB))
	}
}

// TestSend_WritesUserJSONToStdin drives a fake claude and asserts the user
// turn is written as a stream-json message on stdin (the persistent-process
// protocol path).
func TestSend_WritesUserJSONToStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shell shim is unix-only in the migrated tests")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	stdinFile := filepath.Join(workDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"echo \"$line\" > \"$CLAUDE_STDIN_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"session_id\":\"sess-fake\"}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"session_id\":\"sess-fake\"}'\n"
	writeFakeClaudeScript(t, binDir, script)

	t.Setenv("CLAUDE_STDIN_FILE", stdinFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newClaudeSession(context.Background(), workDir, "claude", nil, "claude-opus-4-7", "", "", "default", "", "", nil, nil, nil, nil, 0, "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForDoneResult(t, cs.Events())

	waitForFileContains(t, stdinFile, `"type":"user"`)
	waitForFileContains(t, stdinFile, `"content":"hello"`)
	if cs.CurrentSessionID() != "sess-fake" {
		t.Fatalf("CurrentSessionID() = %q, want sess-fake (from system event)", cs.CurrentSessionID())
	}
}

// TestSend_MultiTurnPersistentProcess proves the persistent-process lifecycle:
// two turns are written to the SAME process's stdin (no relaunch), and the
// events channel stays open across turns.
func TestSend_MultiTurnPersistentProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shell shim is unix-only in the migrated tests")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	stdinFile := filepath.Join(workDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"while IFS= read -r line; do echo \"$line\" >> \"$CLAUDE_STDIN_FILE\"; done\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"session_id\":\"sess-persist\"}'\n"
	writeFakeClaudeScript(t, binDir, script)

	t.Setenv("CLAUDE_STDIN_FILE", stdinFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newClaudeSession(context.Background(), workDir, "claude", nil, "", "", "", "default", "", "", nil, nil, nil, nil, 0, "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "first"}}}); err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	waitForFileLines(t, stdinFile, 1)

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "second"}}}); err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	waitForFileLines(t, stdinFile, 2)

	data, _ := os.ReadFile(stdinFile)
	if !strings.Contains(string(data), `"content":"first"`) || !strings.Contains(string(data), `"content":"second"`) {
		t.Fatalf("stdin missing both turns: %q", string(data))
	}
}

// TestAbort_KeepsPersistentProcessAlive is the regression test for F3: Abort
// must cancel the in-flight turn WITHOUT killing the persistent Claude process
// or destroying the session, so a subsequent turn proceeds on the SAME
// process. (Previously Abort cancelled the session context and killed the
// CLI, which destroyed the whole persistent session and lost native
// conversation history on every client disconnect.)
func TestAbort_KeepsPersistentProcessAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	stdinFile := filepath.Join(workDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"while IFS= read -r line; do echo \"$line\" >> \"$CLAUDE_STDIN_FILE\"; done\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"session_id\":\"sess-persist\"}'\n"
	writeFakeClaudeScript(t, binDir, script)

	t.Setenv("CLAUDE_STDIN_FILE", stdinFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newClaudeSession(context.Background(), workDir, "claude", nil, "", "", "", "default", "", "", nil, nil, nil, nil, 0, "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	defer cs.Close(context.Background())

	// Turn 1 is delivered to the persistent process.
	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "first"}}}); err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	waitForFileLines(t, stdinFile, 1)

	// Abort must NOT destroy the process or the session.
	if err := cs.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !cs.Alive() {
		t.Fatal("Abort killed the persistent process; the session is destroyed")
	}
	// The event stream must still be open (session not torn down).
	select {
	case _, ok := <-cs.Events():
		if !ok {
			t.Fatal("Abort closed the session event stream; the session is destroyed")
		}
	case <-time.After(100 * time.Millisecond):
		// Channel open with no event yet — process still alive and serving.
	}

	// Turn 2 proceeds on the SAME process.
	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "second"}}}); err != nil {
		t.Fatalf("Send(second) after Abort: %v", err)
	}
	waitForFileLines(t, stdinFile, 2)

	data, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin file: %v", err)
	}
	if !strings.Contains(string(data), `"content":"first"`) || !strings.Contains(string(data), `"content":"second"`) {
		t.Fatalf("stdin missing both turns after Abort: %q", string(data))
	}
}

// TestClose_ForceKillsProcessGroupAfterGracefulTimeout exercises the graceful
// escalation: a CLI that ignores stdin EOF must be SIGTERM'd/SIGKILL'd as a
// group, and Close must not hang.
func TestClose_ForceKillsProcessGroupAfterGracefulTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	script := "#!/bin/sh\nsleep 30\n"
	writeFakeClaudeScript(t, binDir, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newClaudeSession(context.Background(), workDir, "claude", nil, "", "", "", "default", "", "", nil, nil, nil, nil, 0, "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	cs.gracefulStopTimeout = 50 * time.Millisecond

	closeStarted := time.Now()
	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(closeStarted); elapsed > 3*time.Second {
		t.Fatalf("Close took too long after force kill: %v", elapsed)
	}
}

func TestSessionSend_AfterClose_ReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shell shim is unix-only in the migrated tests")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"session_id\":\"x\"}'\nprintf '%s\\n' '{\"type\":\"result\"}'\n"
	writeFakeClaudeScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newClaudeSession(context.Background(), workDir, "claude", nil, "", "", "", "default", "", "", nil, nil, nil, nil, 0, "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Send after Close err = %v, want 'process is not running'", err)
	}
}

func TestDescribe_DeclaresPersistentProcessLifecycle(t *testing.T) {
	a, err := NewAdapter(Options{Command: "claude"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "claude-code" {
		t.Fatalf("ModelID = %q, want claude-code", desc.ModelID)
	}
	if desc.LifecycleMode != gwrt.LifecyclePersistentProcess {
		t.Fatalf("LifecycleMode = %q, want %q", desc.LifecycleMode, gwrt.LifecyclePersistentProcess)
	}
	if !desc.Capabilities.Streaming || !desc.Capabilities.ToolCalls || !desc.Capabilities.Reasoning ||
		!desc.Capabilities.Permission || !desc.Capabilities.Resume || !desc.Capabilities.MultiTurn {
		t.Fatalf("Capabilities = %+v, want all capabilities enabled", desc.Capabilities)
	}
}

func TestAdapterStart_ResumeIDFromMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shell shim is unix-only in the migrated tests")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"session_id\":\"native-sess\"}'\nprintf '%s\\n' '{\"type\":\"result\"}'\n"
	writeFakeClaudeScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GW_WORKSPACE_DIR", workDir)

	a, err := NewAdapter(Options{Command: "claude"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	sess, err := a.Start(context.Background(), gwrt.StartRequest{
		ModelID:     "claude-code",
		SessionID:   "sess-1",
		CallerID:    "owner-1",
		WorkspaceID: "ws-1",
		Metadata:    map[string]string{"claude_session_id": "native-sess"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(context.Background())

	cs, ok := sess.(*claudeSession)
	if !ok {
		t.Fatalf("session type = %T, want *claudeSession", sess)
	}
	if got := cs.CurrentSessionID(); got != "native-sess" {
		t.Fatalf("CurrentSessionID() = %q, want native-sess (native resume from metadata)", got)
	}
}

func TestRegistry_RegistersClaudeCode(t *testing.T) {
	names := gwrt.List()
	for _, n := range names {
		if n == "claude-code" {
			return
		}
	}
	t.Fatalf("gwrt.List() = %v, want it to contain claude-code", names)
}

func TestNew_CommandNotFound(t *testing.T) {
	t.Setenv("CC_GATEWAY_CLAUDE_COMMAND", "definitely-not-a-real-claude-binary-xyz")
	_, err := New(context.Background(), "claude-code")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("New with bad command err = %v, want 'not found in PATH'", err)
	}
}

func TestFactory_ResolvesAdapter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shell shim is unix-only in the migrated tests")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"session_id\":\"x\"}'\nprintf '%s\\n' '{\"type\":\"result\"}'\n"
	writeFakeClaudeScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a, err := gwrt.Resolve(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "claude-code" {
		t.Fatalf("ModelID = %q, want claude-code", desc.ModelID)
	}
}

// ---------------------------------------------------------------------------
// Shared fake-claude helpers (ported from the upstream agent/claudecode
// session_test.go / claudecode_test.go).
// ---------------------------------------------------------------------------

func writeFakeClaudeScript(t *testing.T, dir, shellScript string) {
	t.Helper()
	scriptPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(scriptPath, []byte(shellScript), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
}

func newTestClaudeSession(t *testing.T) *claudeSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := &claudeSession{
		events: make(chan gwrt.Event, 64),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	cs.setPermissionMode("default")
	return cs
}

func waitForDoneResult(t *testing.T, events <-chan gwrt.Event) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before finish")
			}
			if evt.Type == gwrt.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == gwrt.EventFinish {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for finish")
		}
	}
}

// waitForFileContains polls a file until it contains the wanted substring.
func waitForFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("file %s: got %q, want substring %q", path, string(data), want)
}

func waitForFileLines(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			count := 0
			for _, line := range lines {
				if strings.TrimSpace(line) != "" {
					count++
				}
			}
			if count >= want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d lines in %s", want, path)
}

func containsSequence(args, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
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

func helperCommand(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", mode)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	return cmd
}

// TestHelperProcess lets this test binary act as a tiny external command for
// cases that need a process with controlled lifetime semantics.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "err-then-sleep":
		_, _ = os.Stderr.WriteString("helper: starting up\n")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "stdin-eof-exit":
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}
