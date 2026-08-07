// Kimi adapter tests — migrated from the upstream in-repo agent/kimi package
// (originally cc-connect) and adapted to the new runtime contract.
//
// These tests cover CLI flag detection (--print probe), stream-json event
// mapping, native session resume, abort, abnormal exit, session lifecycle and
// argument building. They never require a real Kimi login: every subprocess
// test drives a fake `kimi` shell script whose stdout is the canonical
// stream-json line format. IM/provider-specific tests were not migrated.
package kimi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gwrt "github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"default", "default"},
		{"DEFAULT", "default"},
		{"yolo", "yolo"},
		{"YOLO", "yolo"},
		{"force", "yolo"},
		{"bypass", "yolo"},
		{"auto", "yolo"},
		{"plan", "plan"},
		{"quiet", "quiet"},
		{"", "default"},
		{"unknown", "default"},
	}

	for _, c := range cases {
		if got := normalizeMode(c.input); got != c.expected {
			t.Fatalf("normalizeMode(%q) = %q, want %q", c.input, got, c.expected)
		}
	}
}

func TestExtractResumeSessionID(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"To resume this session: kimi -r e3690555-60eb-4d50-874b-e3647e9cee5b", "e3690555-60eb-4d50-874b-e3647e9cee5b"},
		{"To resume this session: kimi --resume abc-def", ""},
		{"To resume this session: no-id-here", ""},
		{"random text", ""},
	}

	for _, c := range cases {
		if got := extractResumeSessionID(c.input); got != c.expected {
			t.Fatalf("extractResumeSessionID(%q) = %q, want %q", c.input, got, c.expected)
		}
	}
}

func TestHandleAssistantWithText(t *testing.T) {
	ks := newTestKimiSession(t, "")
	ks.handleEvent(map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello!"},
		},
	})

	// pendingMsgs should buffer the text.
	if len(ks.pendingMsgs) != 1 || ks.pendingMsgs[0] != "Hello!" {
		t.Fatalf("pendingMsgs = %v, want [Hello!]", ks.pendingMsgs)
	}
}

func TestHandleAssistantWithThink(t *testing.T) {
	ks := newTestKimiSession(t, "")
	ks.handleEvent(map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "think", "think": "Let me think..."},
			map[string]any{"type": "text", "text": "Done!"},
		},
	})

	events := drainEvents(ks.events, 2)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1 (think) event", events)
	}
	// The runtime contract has no separate thinking type; think blocks ride
	// on EventText (same policy as the codex adapter).
	if events[0].Type != gwrt.EventText || events[0].Text != "Let me think..." {
		t.Fatalf("events[0] = %+v, want text/Let me think...", events[0])
	}
	if len(ks.pendingMsgs) != 1 || ks.pendingMsgs[0] != "Done!" {
		t.Fatalf("pendingMsgs = %v, want [Done!]", ks.pendingMsgs)
	}
}

func TestHandleAssistantWithToolCalls(t *testing.T) {
	ks := newTestKimiSession(t, "")
	ks.handleEvent(map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "I will run a command"},
		},
		"tool_calls": []any{
			map[string]any{
				"id": "tool_abc",
				"function": map[string]any{
					"name":      "Shell",
					"arguments": `{"command":"echo hello"}`,
				},
			},
		},
	})

	events := drainEvents(ks.events, 3)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2 (text flush + tool use)", events)
	}
	if events[0].Type != gwrt.EventText || events[0].Text != "I will run a command" {
		t.Fatalf("events[0] = %+v, want pre-tool text flush", events[0])
	}
	if events[1].Type != gwrt.EventToolUse || events[1].Tool == nil {
		t.Fatalf("events[1] = %+v, want EventToolUse", events[1])
	}
	if events[1].Tool.Name != "Shell" {
		t.Fatalf("tool name = %q, want Shell", events[1].Tool.Name)
	}
	if events[1].Tool.ID != "tool_abc" {
		t.Fatalf("tool id = %q, want tool_abc", events[1].Tool.ID)
	}
	if cmd, _ := events[1].Tool.Arguments["command"].(string); cmd != "echo hello" {
		t.Fatalf("tool arguments = %v, want command echo hello", events[1].Tool.Arguments)
	}
}

func TestHandleTool(t *testing.T) {
	ks := newTestKimiSession(t, "")
	ks.handleEvent(map[string]any{
		"role":         "tool",
		"tool_call_id": "tool_abc",
		"content": []any{
			map[string]any{"type": "text", "text": "hello\n"},
		},
	})

	events := drainEvents(ks.events, 1)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1", events)
	}
	if events[0].Type != gwrt.EventToolResult || events[0].Tool == nil {
		t.Fatalf("events[0] = %+v, want EventToolResult", events[0])
	}
	if events[0].Tool.ID != "tool_abc" {
		t.Fatalf("tool result id = %q, want tool_abc", events[0].Tool.ID)
	}
	if !strings.Contains(events[0].Tool.Result, "hello") {
		t.Fatalf("tool result = %q, want to contain hello", events[0].Tool.Result)
	}
}

func TestFlushPendingAsText(t *testing.T) {
	ks := newTestKimiSession(t, "")
	ks.pendingMsgs = []string{"Hello", " ", "world"}
	ks.flushPendingAsText()

	events := drainEvents(ks.events, 1)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1", events)
	}
	if events[0].Type != gwrt.EventText || events[0].Text != "Hello world" {
		t.Fatalf("events[0] = %+v, want text/Hello world", events[0])
	}
	if len(ks.pendingMsgs) != 0 {
		t.Fatalf("pendingMsgs not cleared: %v", ks.pendingMsgs)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("truncate(hello,10) = %q", got)
	}
	if got := truncate("hello world", 11); got != "hello world" {
		t.Fatalf("truncate(hello world,11) = %q", got)
	}
	if got := truncate("hello world", 10); got != "hello worl..." {
		t.Fatalf("truncate(hello world,10) = %q", got)
	}
}

// TestBuildArgs_NoPrintSupportOmitsPrintFlag is the regression test for #1456.
// When the locally installed Kimi CLI does not advertise --print in its help
// output, the adapter must omit that flag — otherwise the newer Kimi Code CLI
// exits with `error: unknown option '--print' (Did you mean --prompt?)`.
func TestBuildArgs_NoPrintSupportOmitsPrintFlag(t *testing.T) {
	ks, err := newKimiSession(context.Background(), "kimi", nil, "/tmp", "", "default", "", nil, 0, kimiFlagSupport{Print: false})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	args := ks.buildArgs("hello")

	for _, a := range args {
		if a == "--print" {
			t.Fatalf("buildArgs unexpectedly emitted --print when flagSupport.Print=false; args=%v", args)
		}
	}
	if !containsArg(args, "--prompt") {
		t.Fatalf("args missing --prompt; args=%v", args)
	}
	if !containsArg(args, "hello") {
		t.Fatalf("args missing prompt text; args=%v", args)
	}
}

// TestBuildArgs_PrintSupportIncludesPrintFlag covers the legacy kimi-cli
// branch — the binary advertises --print, so we must keep emitting it for
// --output-format stream-json to take effect.
func TestBuildArgs_PrintSupportIncludesPrintFlag(t *testing.T) {
	ks, err := newKimiSession(context.Background(), "kimi", nil, "/tmp", "", "default", "", nil, 0, kimiFlagSupport{Print: true})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	args := ks.buildArgs("hello")
	if !containsArg(args, "--print") {
		t.Fatalf("args missing --print; args=%v", args)
	}
	if !containsSequence(args, []string{"--output-format", "stream-json"}) {
		t.Fatalf("args missing --output-format stream-json; args=%v", args)
	}
}

// TestBuildArgs_PlanMode confirms plan mode still passes --plan independent
// of --print support.
func TestBuildArgs_PlanMode(t *testing.T) {
	ks, err := newKimiSession(context.Background(), "kimi", nil, "/tmp", "kimi-k2", "plan", "", nil, 0, kimiFlagSupport{Print: false})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	args := ks.buildArgs("plan this")
	if !containsArg(args, "--plan") {
		t.Fatalf("args missing --plan; args=%v", args)
	}
	if !containsSequence(args, []string{"--model", "kimi-k2"}) {
		t.Fatalf("args missing --model kimi-k2; args=%v", args)
	}
}

// TestBuildArgs_ResumeSession ensures session continuity flags are emitted
// regardless of the --print probe result.
func TestBuildArgs_ResumeSession(t *testing.T) {
	ks, err := newKimiSession(context.Background(), "kimi", nil, "/tmp", "", "default", "sess-xyz", nil, 0, kimiFlagSupport{Print: false})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	args := ks.buildArgs("continue")
	resumeIdx := indexOf(args, "--resume")
	if resumeIdx < 0 {
		t.Fatalf("args missing --resume; args=%v", args)
	}
	if resumeIdx+1 >= len(args) || args[resumeIdx+1] != "sess-xyz" {
		t.Fatalf("--resume not followed by id; args=%v", args)
	}
}

// TestParseKimiHelpFlags_LegacyAdvertisesPrint covers the legacy kimi-cli
// help layout (Typer/click box style) advertising --print.
func TestParseKimiHelpFlags_LegacyAdvertisesPrint(t *testing.T) {
	flags := parseKimiHelpFlags(legacyKimiHelp)
	if !flags["--print"] {
		t.Fatal("legacy help text must advertise --print")
	}
	if !flags["--prompt"] {
		t.Fatal("legacy help text must advertise --prompt")
	}
	if !flags["--output-format"] {
		t.Fatal("legacy help text must advertise --output-format")
	}
	if !flags["--thinking"] || !flags["--no-thinking"] {
		t.Fatal("alias-split should pick up --thinking/--no-thinking")
	}
}

// TestParseKimiHelpFlags_ModernHidesPrint is the regression for #1456: the
// newer Kimi Code CLI help text must not be detected as supporting --print.
func TestParseKimiHelpFlags_ModernHidesPrint(t *testing.T) {
	flags := parseKimiHelpFlags(modernKimiHelp)
	if flags["--print"] {
		t.Fatal("modern help text must not advertise --print")
	}
	if !flags["--prompt"] {
		t.Fatal("modern CLI still advertises --prompt")
	}
	if !flags["--output-format"] {
		t.Fatal("modern CLI still advertises --output-format")
	}
}

func TestParseKimiHelpFlags_IgnoresPositionalAndShortOnly(t *testing.T) {
	help := `
  Arguments:
    COMMAND   Optional sub-command

  Options:
    -h        Show short-only help (must be ignored)
    --        Bare double-dash (must be ignored)
    --debug   Toggle debug mode
`
	flags := parseKimiHelpFlags(help)
	if !flags["--debug"] {
		t.Fatal("--debug must be detected")
	}
	if flags["--"] {
		t.Fatal("bare -- must not be treated as a flag")
	}
	if flags["-h"] {
		t.Fatal("short-only flags are not in the long-flag set")
	}
}

// TestProbeKimiFlags_FallbackOnMissingBinary ensures probeKimiFlags never
// panics on a missing binary and returns the conservative zero value (the
// modern CLI surface without --print).
func TestProbeKimiFlags_FallbackOnMissingBinary(t *testing.T) {
	got := probeKimiFlags(context.Background(),
		"kimi-binary-that-does-not-exist-1456-test",
		200*time.Millisecond)
	if got != (kimiFlagSupport{}) {
		t.Fatalf("probe on missing binary = %+v, want zero value", got)
	}
}

// TestProbeKimiFlags_DetectsPrintFromFakeCLI drives the probe against a fake
// `kimi --help` that advertises --print, proving the probe path works end to
// end without a real binary.
func TestProbeKimiFlags_DetectsPrintFromFakeCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kimi shell shim is unix-only in the migrated tests")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + legacyKimiHelp + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(binDir, "kimi"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kimi: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	support := probeKimiFlags(context.Background(), "kimi", 2*time.Second)
	if !support.Print {
		t.Fatal("probe against fake legacy kimi should detect --print support")
	}
}

func TestPromptFromInput_ResumeUsesLastUserMessage(t *testing.T) {
	in := gwrt.Input{Messages: []gwrt.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second"},
	}}
	if got := promptFromInput(in, true); got != "second" {
		t.Fatalf("resume prompt = %q, want second (kimi owns history on resume)", got)
	}
}

func TestPromptFromInput_FreshIncludesSystem(t *testing.T) {
	in := gwrt.Input{Messages: []gwrt.Message{
		{Role: "system", Content: "You are a senior Rust engineer."},
		{Role: "user", Content: "Review this code"},
	}}
	got := promptFromInput(in, false)
	if !strings.Contains(got, "System instructions:\nYou are a senior Rust engineer.") {
		t.Fatalf("fresh prompt missing system message: %q", got)
	}
	if !strings.Contains(got, "Review this code") {
		t.Fatalf("fresh prompt missing user message: %q", got)
	}
}

// TestSend_ResumeFlow_StartsFreshThenResumes drives two turns against a fake
// kimi: the first launches without --resume and the stream's "To resume this
// session" trailer stores the native session id; the second turn launches
// with --resume <id>.
func TestSend_ResumeFlow_StartsFreshThenResumes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kimi shell shim is unix-only in the migrated tests")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$KIMI_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}'\n" +
		"printf '%s\\n' 'To resume this session: kimi -r sess-1234' >&2\n"
	writeFakeKimiScript(t, binDir, script)

	t.Setenv("KIMI_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ks, err := newKimiSession(context.Background(), "kimi", nil, workDir, "", "default", "", nil, 0, kimiFlagSupport{Print: true})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	if err := ks.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "first"}}}); err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	waitForDoneResult(t, ks.Events())
	waitForSessionID(t, ks, "sess-1234")

	if err := ks.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "second"}}}); err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	waitForDoneResult(t, ks.Events())

	args := waitForArgsFile(t, argsFile)
	if countSequence(args, []string{"--resume", "sess-1234"}) < 1 {
		t.Fatalf("args missing --resume sess-1234 on the second turn; args=%v", args)
	}
}

// TestSend_EmitsEventsInOrder drives a fake kimi whose stream interleaves an
// assistant text block, a tool_call, a tool result, and the final assistant
// text. The pre-tool buffered commentary must be emitted BEFORE the tool use
// (the proven upstream ordering), and the final text must still arrive.
func TestSend_EmitsEventsInOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kimi shell shim is unix-only in the migrated tests")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	payload := strings.Join([]string{
		`{"role":"assistant","content":[{"type":"text","text":"checking constraints"}],"tool_calls":[{"id":"tool_1","function":{"name":"Shell","arguments":"{\"command\":\"ls\"}"}}]}`,
		`{"role":"tool","tool_call_id":"tool_1","content":[{"type":"text","text":"file1"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"final answer"}]}`,
	}, "\n") + "\n"
	payloadFile := filepath.Join(workDir, "payload.jsonl")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	script := "#!/bin/sh\ncat \"$KIMI_PAYLOAD_FILE\"\n"
	writeFakeKimiScript(t, binDir, script)

	t.Setenv("KIMI_PAYLOAD_FILE", payloadFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ks, err := newKimiSession(context.Background(), "kimi", nil, workDir, "", "default", "", nil, 0, kimiFlagSupport{})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	if err := ks.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var order []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-ks.Events():
			if !ok {
				t.Fatal("events channel closed before finish")
			}
			if evt.Type == gwrt.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			switch evt.Type {
			case gwrt.EventText:
				order = append(order, "text:"+evt.Text)
			case gwrt.EventToolUse:
				order = append(order, "tool:"+evt.Tool.Name)
			case gwrt.EventToolResult:
				order = append(order, "result:"+evt.Tool.ID)
			case gwrt.EventFinish:
				want := []string{"text:checking constraints", "tool:Shell", "result:tool_1", "text:final answer"}
				if len(order) != len(want) {
					t.Fatalf("event order = %v, want %v", order, want)
				}
				for i := range want {
					if order[i] != want[i] {
						t.Fatalf("event order = %v, want %v", order, want)
					}
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}
}

// TestAbort_KillsInFlightProcess proves Abort terminates the per-turn kimi
// process so Close completes quickly; the session stays alive for the next
// turn (resume_per_turn lifecycle).
func TestAbort_KillsInFlightProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	script := "#!/bin/sh\nprintf '%s\\n' '{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"starting\"}]}'\nsleep 30\n"
	writeFakeKimiScript(t, binDir, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ks, err := newKimiSession(context.Background(), "kimi", nil, workDir, "", "default", "", nil, 0, kimiFlagSupport{})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	defer ks.Close(context.Background())

	if err := ks.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Wait until the process is actually running and in flight.
	waitForInFlight(t, ks)

	if err := ks.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- ks.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after Abort: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked after Abort; in-flight process was not killed")
	}
}

func TestSessionSend_AfterClose_ReturnsError(t *testing.T) {
	ks, err := newKimiSession(context.Background(), "kimi", nil, t.TempDir(), "", "default", "", nil, 0, kimiFlagSupport{})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	if err := ks.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = ks.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Send after Close err = %v, want 'session is closed'", err)
	}
}

func TestSessionClose_Idempotent(t *testing.T) {
	ks, err := newKimiSession(context.Background(), "kimi", nil, t.TempDir(), "", "default", "", nil, 0, kimiFlagSupport{})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	if err := ks.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ks.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDescribe_DeclaresLifecycleAndCapabilities(t *testing.T) {
	a, err := NewAdapter(Options{Command: "kimi"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "kimi" {
		t.Fatalf("ModelID = %q, want kimi", desc.ModelID)
	}
	if desc.LifecycleMode != gwrt.LifecycleResumePerTurn {
		t.Fatalf("LifecycleMode = %q, want %q", desc.LifecycleMode, gwrt.LifecycleResumePerTurn)
	}
	if !desc.Capabilities.Streaming || !desc.Capabilities.ToolCalls || !desc.Capabilities.Resume || !desc.Capabilities.MultiTurn {
		t.Fatalf("Capabilities = %+v, want streaming/tools/resume/multiturn", desc.Capabilities)
	}
}

func TestAdapterStart_ResumeIDFromMetadata(t *testing.T) {
	t.Setenv("GW_WORKSPACE_DIR", t.TempDir())
	a, err := NewAdapter(Options{Command: "kimi"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	sess, err := a.Start(context.Background(), gwrt.StartRequest{
		ModelID:     "kimi",
		SessionID:   "sess-1",
		OwnerID:     "owner-1",
		WorkspaceID: "ws-1",
		Metadata:    map[string]string{"kimi_session_id": "native-sess"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(context.Background())

	ks, ok := sess.(*kimiSession)
	if !ok {
		t.Fatalf("session type = %T, want *kimiSession", sess)
	}
	if got := ks.CurrentSessionID(); got != "native-sess" {
		t.Fatalf("CurrentSessionID() = %q, want native-sess", got)
	}
}

func TestRegistry_RegistersKimi(t *testing.T) {
	names := gwrt.List()
	for _, n := range names {
		if n == "kimi" {
			return
		}
	}
	t.Fatalf("gwrt.List() = %v, want it to contain kimi", names)
}

func TestNew_CommandNotFound(t *testing.T) {
	t.Setenv("CC_GATEWAY_KIMI_COMMAND", "definitely-not-a-real-kimi-binary-xyz")
	_, err := New(context.Background(), "kimi")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("New with bad command err = %v, want 'not found in PATH'", err)
	}
}

// TestNewAdapter_TimeoutEnvAcceptsBareSeconds is the regression test for the
// CC_GATEWAY_KIMI_TIMEOUT_SECS parse bug: time.ParseDuration("30") fails (it
// requires a unit suffix), so a bare-integer value previously silently fell
// through to Timeout=0 (no per-turn timeout), contradicting the env name.
// Both the bare seconds form and the full duration form must be honored.
func TestNewAdapter_TimeoutEnvAcceptsBareSeconds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kimi shell shim is unix-only in the migrated tests")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}'\n"
	writeFakeKimiScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("bare seconds", func(t *testing.T) {
		t.Setenv("CC_GATEWAY_KIMI_TIMEOUT_SECS", "30")
		a, err := New(context.Background(), "kimi")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := a.(*KimiAdapter).opts.Timeout; got != 30*time.Second {
			t.Fatalf("Timeout = %v, want 30s (bare seconds must be treated as seconds)", got)
		}
	})

	t.Run("duration string", func(t *testing.T) {
		t.Setenv("CC_GATEWAY_KIMI_TIMEOUT_SECS", "45s")
		a, err := New(context.Background(), "kimi")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := a.(*KimiAdapter).opts.Timeout; got != 45*time.Second {
			t.Fatalf("Timeout = %v, want 45s", got)
		}
	})
}

// TestNew_ProbesFlagSupport verifies the factory probes the installed CLI's
// --help surface once and threads the detected --print support into the
// adapter, so Send() can adapt args per installed CLI version (#1456).
func TestNew_ProbesFlagSupport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kimi shell shim is unix-only in the migrated tests")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--help\" ]; then\n" +
		"cat <<'EOF'\n" + legacyKimiHelp + "\nEOF\n" +
		"else\n" +
		"printf '%s\\n' '{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}'\n" +
		"fi\n"
	writeFakeKimiScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a, err := New(context.Background(), "kimi")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adapter, ok := a.(*KimiAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want *KimiAdapter", a)
	}
	if !adapter.flagSupport.Print {
		t.Fatal("factory probe did not detect --print support from the fake legacy kimi")
	}
}

func TestFactory_ResolvesAdapter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kimi shell shim is unix-only in the migrated tests")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}'\n"
	writeFakeKimiScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a, err := gwrt.Resolve(context.Background(), "kimi")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "kimi" {
		t.Fatalf("ModelID = %q, want kimi", desc.ModelID)
	}
}

// ---------------------------------------------------------------------------
// Shared fake-kimi helpers (ported from the upstream agent/kimi tests).
// ---------------------------------------------------------------------------

func writeFakeKimiScript(t *testing.T, dir, shellScript string) {
	t.Helper()
	scriptPath := filepath.Join(dir, "kimi")
	if err := os.WriteFile(scriptPath, []byte(shellScript), 0o755); err != nil {
		t.Fatalf("write fake kimi: %v", err)
	}
}

func newTestKimiSession(t *testing.T, resumeID string) *kimiSession {
	t.Helper()
	ks, err := newKimiSession(context.Background(), "kimi", nil, t.TempDir(), "", "default", resumeID, nil, 0, kimiFlagSupport{})
	if err != nil {
		t.Fatalf("newKimiSession: %v", err)
	}
	t.Cleanup(func() { ks.Close(context.Background()) })
	return ks
}

func drainEvents(ch <-chan gwrt.Event, max int) []gwrt.Event {
	var events []gwrt.Event
	timeout := time.After(500 * time.Millisecond)
	for i := 0; i < max; i++ {
		select {
		case evt := <-ch:
			events = append(events, evt)
		case <-timeout:
			return events
		}
	}
	return events
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

func waitForSessionID(t *testing.T, ks *kimiSession, want string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case <-time.After(10 * time.Millisecond):
			if ks.CurrentSessionID() == want {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for session id %q", want)
		}
	}
}

func waitForInFlight(t *testing.T, ks *kimiSession) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case <-time.After(10 * time.Millisecond):
			if ks.inFlightCmd() != nil {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for in-flight process")
		}
	}
}

func readArgsFile(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // not written yet; callers poll
	}
	lines := strings.Split(string(data), "\n")
	args := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

func waitForArgsFile(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		args := readArgsFile(t, path)
		if len(args) > 0 {
			return args
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for non-empty args file: %s", path)
	return nil
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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

func countSequence(args, want []string) int {
	count := 0
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

func indexOf(args []string, target string) int {
	for i, arg := range args {
		if arg == target {
			return i
		}
	}
	return -1
}

// legacyKimiHelp is a representative slice of the older kimi-cli `--help`
// output (still on the kimi-cli `main` branch as of 2026). It advertises
// both `--print` and `--prompt`.
const legacyKimiHelp = `
 Usage: kimi [OPTIONS] COMMAND [ARGS]...

 Kimi, your next CLI agent.

 ╭─ Options ──────────────────────────────────────────────────────────────╮
 │ --version          -V          Show version and exit.                  │
 │ --work-dir         -w DIRECTORY Working directory for the agent.       │
 │ --session          -S [ID]     Resume a session.                       │
 │ --continue         -C          Continue the previous session.          │
 │ --model            -m TEXT     LLM model to use.                       │
 │ --thinking/--no-thinking       Enable thinking mode.                   │
 │ --yolo             -y          Automatically approve all actions.      │
 │ --plan                         Start in plan mode.                     │
 │ --prompt           -p TEXT     User prompt to the agent.               │
 │ --print                        Run in print mode (non-interactive).    │
 │ --output-format    FORMAT      Output format to use.                   │
 │ --quiet                        Alias for --print --output-format text. │
 ╰────────────────────────────────────────────────────────────────────────╯
`

// modernKimiHelp emulates the newer Kimi Code CLI, where --print has been
// removed entirely. This is the surface that triggers #1456 today.
const modernKimiHelp = `
 Usage: kimi [OPTIONS] COMMAND [ARGS]...

 Kimi, your next CLI agent.

 Options:
   -V, --version              Show version and exit.
   -w, --work-dir DIRECTORY   Working directory for the agent.
   -S, --session [ID]         Resume a session.
   -c, --continue             Continue the most recent session.
   -m, --model TEXT           LLM model to use.
   -p, --prompt TEXT          Run a single prompt non-interactively.
   --output-format FORMAT     Non-interactive output format.
   -y, --yolo                 Auto-approve regular tool calls.
   --plan                     Start a new session in Plan mode.
`
