// Codex adapter tests — migrated from the upstream in-repo agent/codex
// package (originally cc-connect) and adapted to the new runtime contract.
// These tests cover CLI launch/args, JSON stream event mapping, native session
// ID resume, usage from rollout files, permission event emission, abort, and
// process-group teardown. They never require a real Codex login: every
// subprocess test drives a fake `codex` shell script whose stdout is the
// canonical JSONL stream.
package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gwrt "github.com/tianlinzz/agent-cli-gateway/runtime"
)

func TestNormalizeReasoningEffort_RejectsMinimal(t *testing.T) {
	if got := normalizeReasoningEffort("minimal"); got != "" {
		t.Fatalf("normalizeReasoningEffort(minimal) = %q, want empty", got)
	}
	if got := normalizeReasoningEffort("min"); got != "" {
		t.Fatalf("normalizeReasoningEffort(min) = %q, want empty", got)
	}
}

func TestNormalizeMode_MapsAliases(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"suggest", "suggest"},
		{"auto-edit", "auto-edit"},
		{"autoedit", "auto-edit"},
		{"full-auto", "full-auto"},
		{"fullauto", "full-auto"},
		{"auto", "full-auto"},
		{"yolo", "yolo"},
		{"", "suggest"},
	} {
		if got := normalizeMode(tc.in); got != tc.want {
			t.Fatalf("normalizeMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildExecArgs_IncludesReasoningEffort(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "o3", "high", "full-auto", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	want := []string{
		"exec",
		"--skip-git-repo-check",
		"--sandbox",
		"danger-full-access",
		"-c",
		`approval_policy="never"`,
		"--model",
		"o3",
		"-c",
		`model_reasoning_effort="high"`,
		"--json",
		"--cd",
		"/tmp/project",
		"-",
	}
	if len(args) != len(want) {
		t.Fatalf("args len = %d, want %d, args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q, args=%v", i, args[i], want[i], args)
		}
	}
}

func TestBuildExecArgs_IncludesBaseURL(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "o3", "high", "full-auto", "", "https://custom.api.example.com", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	if !containsSequence(args, []string{"-c", `openai_base_url="https://custom.api.example.com"`}) {
		t.Fatalf("args missing openai_base_url config flag: %v", args)
	}
}

func TestBuildExecArgs_IncludesModelProvider(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "openai/gpt-5.3-codex", "", "full-auto", "", "https://router.example.com/api/v1", nil, "shengsuanyun", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	if !containsSequence(args, []string{"-c", `model_provider="shengsuanyun"`}) {
		t.Fatalf("args missing model_provider config flag: %v", args)
	}
	if !containsSequence(args, []string{"-c", `openai_base_url="https://router.example.com/api/v1"`}) {
		t.Fatalf("args missing openai_base_url config flag: %v", args)
	}
}

func TestBuildExecArgs_ResumeOmitsCdFlag(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "", "", "full-auto", "thread-abc", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	// codex exec resume does not support --cd; verify it's absent.
	for i, arg := range args {
		if arg == "--cd" {
			t.Fatalf("resume args should not contain --cd, but found at index %d: %v", i, args)
		}
	}

	// --json and stdin marker must still be present.
	if !containsSequence(args, []string{"--json", "-"}) {
		t.Fatalf("resume args missing --json + stdin marker: %v", args)
	}
}

// TestBuildExecArgs_ModeMapping verifies that every mode maps to
// danger-full-access (gateway fork): the deployment target is a container, so
// codex's OS-level sandbox only blocks legitimate tooling with no real security
// gain. approval_policy must always be "never": codex exec has no approval IPC,
// so any other value hangs on a TTY prompt this backend cannot answer.
func TestBuildExecArgs_ModeMapping(t *testing.T) {
	for _, mode := range []string{"suggest", "auto-edit", "full-auto", "yolo", "danger-full-access", ""} {
		t.Run(mode, func(t *testing.T) {
			cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "", "", mode, "", "", nil, "", "", "")
			if err != nil {
				t.Fatalf("newCodexSession: %v", err)
			}
			args := cs.buildExecArgs("hi")

			if !containsSequence(args, []string{"--sandbox", "danger-full-access"}) {
				t.Errorf("mode=%q missing --sandbox danger-full-access; args=%v", mode, args)
			}
			if !containsSequence(args, []string{"-c", `approval_policy="never"`}) {
				t.Errorf("mode=%q missing approval_policy=never; args=%v", mode, args)
			}
			for _, a := range args {
				if a == "--full-auto" {
					t.Errorf("mode=%q still emits deprecated --full-auto; args=%v", mode, args)
				}
			}
		})
	}
}

// TestBuildExecArgs_ResumeUsesSandboxModeConfigOverride is the regression test
// for the "codex exec resume" sandbox flag bug. codex CLI does not accept
// `--sandbox <mode>` on resume (only `codex exec` does); both accept `-c
// key=value`, so resume must express sandbox via `-c sandbox_mode="..."`.
func TestBuildExecArgs_ResumeUsesSandboxModeConfigOverride(t *testing.T) {
	for _, mode := range []string{"suggest", "auto-edit", "full-auto", "yolo", "danger-full-access", ""} {
		t.Run(mode, func(t *testing.T) {
			cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "", "", mode, "thread-abc", "", nil, "", "", "")
			if err != nil {
				t.Fatalf("newCodexSession: %v", err)
			}
			args := cs.buildExecArgs("hi")

			if !containsSequence(args, []string{"exec", "resume", "--skip-git-repo-check"}) {
				t.Fatalf("mode=%q: expected resume invocation, got: %v", mode, args)
			}
			for i, a := range args {
				if a == "--sandbox" {
					t.Errorf("mode=%q: resume args must not contain --sandbox, found at index %d: %v", mode, i, args)
				}
			}
			if !containsSequence(args, []string{"-c", `sandbox_mode="danger-full-access"`}) {
				t.Errorf("mode=%q: resume args missing -c sandbox_mode=...; args=%v", mode, args)
			}
			if !containsSequence(args, []string{"-c", `approval_policy="never"`}) {
				t.Errorf("mode=%q: resume args missing approval_policy=never; args=%v", mode, args)
			}
		})
	}
}

func TestPromptFromInput_FreshPrependsPreamble(t *testing.T) {
	preamble := buildCodexPromptPreamble(
		"You are Linear Reporter.",
		"Always invoke linear-bug-intake.",
	)

	in := gwrt.Input{Messages: []gwrt.Message{
		{Role: "user", Content: "Create a Chat issue."},
	}}
	got := promptFromInput(in, false, preamble)

	for _, want := range []string{
		"Before answering, follow these project-level instructions",
		"Project system prompt:\nYou are Linear Reporter.",
		"Additional project instructions:\nAlways invoke linear-bug-intake.",
		"User message:\nCreate a Chat issue.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestPromptFromInput_EmptyPreambleIsNoop(t *testing.T) {
	in := gwrt.Input{Messages: []gwrt.Message{
		{Role: "user", Content: "Hello"},
	}}
	if got := promptFromInput(in, false, ""); got != "Hello" {
		t.Fatalf("empty preamble changed prompt: %q", got)
	}
}

func TestPromptFromInput_ResumeUsesLastUserMessage(t *testing.T) {
	in := gwrt.Input{Messages: []gwrt.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second"},
	}}
	got := promptFromInput(in, true, "irrelevant preamble")
	if got != "second" {
		t.Fatalf("resume prompt = %q, want %q (codex owns history on resume)", got, "second")
	}
}

func TestPromptFromInput_SystemMessageIncludedOnFresh(t *testing.T) {
	in := gwrt.Input{Messages: []gwrt.Message{
		{Role: "system", Content: "You are a senior Rust engineer."},
		{Role: "user", Content: "Review this code"},
	}}
	got := promptFromInput(in, false, "")
	if !strings.Contains(got, "System instructions:\nYou are a senior Rust engineer.") {
		t.Fatalf("fresh prompt missing system message: %q", got)
	}
	if !strings.Contains(got, "Review this code") {
		t.Fatalf("fresh prompt missing user message: %q", got)
	}
}

func TestSend_EmptyInput_ReturnsError(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, t.TempDir(), "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{}); err == nil {
		t.Fatal("Send with empty input returned nil error, want error")
	}
}

func TestSend_UsesStdinForMultilinePrompt(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	stdinFile := filepath.Join(workDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$CODEX_ARGS_FILE\"\n" +
		"cat > \"$CODEX_STDIN_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-stdin\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\"}'\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("CODEX_ARGS_FILE", argsFile)
	t.Setenv("CODEX_STDIN_FILE", stdinFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "thread-stdin", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	prompt := "line1\nline2"
	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: prompt}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	args := waitForArgsFile(t, argsFile)
	if !containsSequence(args, []string{"--json", "-"}) {
		t.Fatalf("args missing stdin marker: %v", args)
	}

	waitForFileEquals(t, stdinFile, prompt)
}

func TestSend_PrependsProjectPromptOnFreshSession(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	stdinFile := filepath.Join(workDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"cat > \"$CODEX_STDIN_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-preamble\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\"}'\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("CODEX_STDIN_FILE", stdinFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "You are Linear Reporter.", "Always invoke linear-bug-intake.")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer func() { cs.Close(context.Background()) }()

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "Create a Chat issue."}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, want := range []string{
		"Project system prompt:\nYou are Linear Reporter.",
		"Additional project instructions:\nAlways invoke linear-bug-intake.",
		"User message:\nCreate a Chat issue.",
	} {
		waitForFileContains(t, stdinFile, want)
	}
}

func TestSend_HandlesLargeJSONLines(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	largeText := strings.Repeat("x", 11*1024*1024)
	encodedText, err := json.Marshal(largeText)
	if err != nil {
		t.Fatalf("marshal large text: %v", err)
	}

	payload := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-large"}`,
		`{"type":"item.completed","item":{"type":"agent_message","content":[{"type":"output_text","text":` + string(encodedText) + `}]}}`,
		`{"type":"turn.completed"}`,
	}, "\n") + "\n"

	payloadFile := filepath.Join(workDir, "payload.jsonl")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	script := "#!/bin/sh\ncat \"$CODEX_PAYLOAD_FILE\"\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("CODEX_PAYLOAD_FILE", payloadFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var gotTextLen int
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-cs.Events():
			if !ok {
				t.Fatal("events channel closed before finish")
			}
			if evt.Type == gwrt.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == gwrt.EventText {
				gotTextLen = len(evt.Text)
			}
			if evt.Type == gwrt.EventFinish {
				if gotTextLen != len(largeText) {
					t.Fatalf("text len = %d, want %d", gotTextLen, len(largeText))
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for large JSON line events")
		}
	}
}

// TestSend_EmitsAgentMessagesInOrder drives a stream where buffered commentary
// is followed by a tool call: the pre-tool flush must emit the agent_message
// commentary BEFORE the EventToolUse (the proven upstream streaming order), and
// the final answer must still be flushed at turn.completed. This pins the
// migration-fidelity behavior — commentary streams before the tool runs, not
// all at the end of the turn.
func TestSend_EmitsAgentMessagesInOrder(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	payload := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-phase"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"agent_message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"checking constraints"}]}}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"ls -la"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"final answer"}]}}`,
		`{"type":"turn.completed"}`,
	}, "\n") + "\n"

	payloadFile := filepath.Join(workDir, "payload.jsonl")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	script := "#!/bin/sh\ncat \"$CODEX_PAYLOAD_FILE\"\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("CODEX_PAYLOAD_FILE", payloadFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var order []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-cs.Events():
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
			case gwrt.EventFinish:
				want := []string{"text:checking constraints", "tool:Bash", "text:final answer"}
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
			t.Fatal("timed out waiting for agent message events")
		}
	}
}

// TestAbort_DoesNotLoseBufferedCommentary proves the pre-tool flush protects
// already-produced commentary when a turn is aborted mid-tool. The stream
// emits commentary, then a tool item.started (which must flush the buffered
// commentary BEFORE announcing the tool), then the CLI stalls — turn.completed
// never arrives. Abort kills the process; the commentary must already have been
// delivered rather than sitting in the pendingMsgs buffer where it would be
// lost.
func TestAbort_DoesNotLoseBufferedCommentary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-abort-msg\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.started\"}'\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"role\":\"assistant\",\"phase\":\"commentary\",\"content\":[{\"type\":\"output_text\",\"text\":\"checking constraints\"}]}}'\n" +
		"printf '%s\\n' '{\"type\":\"item.started\",\"item\":{\"type\":\"command_execution\",\"command\":\"ls\"}}'\n" +
		"sleep 30\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForThreadID(t, cs, "thread-abort-msg")

	// Drain events until the tool use is announced. With the pre-tool flush the
	// commentary text has already been delivered BEFORE the tool event.
	var order []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-cs.Events():
			if !ok {
				t.Fatal("events channel closed before tool use")
			}
			switch evt.Type {
			case gwrt.EventText:
				order = append(order, "text:"+evt.Text)
			case gwrt.EventToolUse:
				order = append(order, "tool:"+evt.Tool.Name)
				goto toolSeen
			}
		case <-timeout:
			t.Fatal("timed out waiting for tool use event")
		}
	}
toolSeen:

	// Abort mid-tool: turn.completed never arrives, so this is exactly when a
	// deferred final flush would drop the buffered commentary.
	if err := cs.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cs.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after Abort: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked after Abort; in-flight process was not killed")
	}

	if len(order) != 2 || order[0] != "text:checking constraints" || order[1] != "tool:Bash" {
		t.Fatalf("event order = %v, want [text:checking constraints tool:Bash] (commentary must be flushed pre-tool and survive the abort)", order)
	}
}

// TestHandleEvent_MapsStreamToCanonicalEvents feeds the full native event
// stream (thread, tool use, tool result, reasoning, agent message, turn end)
// through handleEvent and asserts the canonical runtime events produced.
func TestHandleEvent_MapsStreamToCanonicalEvents(t *testing.T) {
	// Keep usage lookup out of the real home dir.
	t.Setenv("CODEX_HOME", t.TempDir())

	cs, err := newCodexSession(context.Background(), "codex", nil, "/tmp/project", "", "", "full-auto", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	lines := []string{
		`{"type":"thread.started","thread_id":"thread-events"}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"ls -la"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"ls -la","status":"completed","exit_code":0,"aggregated_output":"file1\nfile2"}}`,
		`{"type":"item.completed","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking hard"}]}}`,
		`{"type":"item.completed","item":{"type":"agent_message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"done"}]}}`,
		`{"type":"turn.completed"}`,
	}
	for _, line := range lines {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("unmarshal fixture line: %v", err)
		}
		cs.handleEvent(raw)
	}

	var got []gwrt.Event
	timeout := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-cs.Events():
			if !ok {
				t.Fatalf("events closed after %d events, want finish: %v", len(got), got)
			}
			got = append(got, evt)
			if evt.Type == gwrt.EventFinish {
				goto done
			}
		case <-timeout:
			t.Fatalf("timed out waiting for finish; got %v", got)
		}
	}
done:
	if cs.CurrentSessionID() != "thread-events" {
		t.Fatalf("CurrentSessionID() = %q, want thread-events", cs.CurrentSessionID())
	}

	toolUse := got[0]
	if toolUse.Type != gwrt.EventToolUse || toolUse.Tool == nil || toolUse.Tool.Name != "Bash" {
		t.Fatalf("got[0] = %+v, want Bash EventToolUse", toolUse)
	}
	if cmd, _ := toolUse.Tool.Arguments["command"].(string); cmd != "ls -la" {
		t.Fatalf("tool use command = %q, want ls -la", cmd)
	}

	toolResult := got[1]
	if toolResult.Type != gwrt.EventToolResult || toolResult.Tool == nil || toolResult.Tool.Name != "Bash" {
		t.Fatalf("got[1] = %+v, want Bash EventToolResult", toolResult)
	}
	if toolResult.Tool.IsError {
		t.Fatalf("tool result IsError = true, want false")
	}
	if !strings.Contains(toolResult.Tool.Result, "file1") {
		t.Fatalf("tool result = %q, want file1 in output", toolResult.Tool.Result)
	}

	if got[2].Type != gwrt.EventText || got[2].Text != "thinking hard" {
		t.Fatalf("got[2] = %+v, want reasoning text event", got[2])
	}
	if got[3].Type != gwrt.EventText || got[3].Text != "done" {
		t.Fatalf("got[3] = %+v, want final text event", got[3])
	}
	if got[4].Type != gwrt.EventFinish || got[4].FinishReason != "end_turn" {
		t.Fatalf("got[4] = %+v, want finish(end_turn)", got[4])
	}
}

// TestSend_ResumeFlow_StartsFreshThenResumes drives two turns against a fake
// codex: the first launches `codex exec` (fresh), the thread.started event
// stores the native thread id, and the second turn launches `codex exec resume
// <thread_id>`.
func TestSend_ResumeFlow_StartsFreshThenResumes(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$CODEX_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-resume\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\"}'\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("CODEX_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "first"}}}); err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	waitForThreadID(t, cs, "thread-resume")
	waitForDoneResult(t, cs.Events())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "second"}}}); err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	waitForDoneResult(t, cs.Events())

	args := waitForArgsFile(t, argsFile)
	if !containsSequence(args, []string{"exec", "resume", "--skip-git-repo-check"}) {
		t.Fatalf("args missing resume invocation after second turn: %v", args)
	}
	if indexOf(args, "thread-resume") == -1 {
		t.Fatalf("args missing native thread id thread-resume: %v", args)
	}
}

// TestAbort_KillsInFlightProcess exercises the abort race the post-fix re-kill
// loop exists to close: the CLI forks a helper concurrently with the abort, the
// first group SIGKILL misses it, and the orphan — eventually in the same
// process group, still holding the stdout pipe — stalls the read loop and would
// block Close.
//
// The fake CLI is a compiled helper whose keeper subprocess is born into its
// OWN process group, so the abort's first group SIGKILL deterministically
// misses it. The keeper detects the direct child's death (the abort's first
// kill reparents the keeper to launchd) and THEN joins the direct child's
// process group. The post-fix Abort keeps re-killing the group until the direct
// child is reaped, so it deterministically catches the keeper. The pre-fix
// single group-kill leaves the keeper orphaned, the read loop never sees EOF on
// the pipe, the direct child is never reaped, and Close times out — a
// deterministic failure on pre-fix code.
func TestAbort_KillsInFlightProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	buildFakeCodexAbortHelper(t, binDir)

	// Widen the re-kill window so the keeper's join (which happens a moment
	// after the first kill lands) is comfortably inside it on any machine.
	oldRetries := codexAbortKillRetries
	oldRetryDelay := codexAbortKillRetryDelay
	codexAbortKillRetries = 40
	codexAbortKillRetryDelay = 25 * time.Millisecond
	t.Cleanup(func() {
		codexAbortKillRetries = oldRetries
		codexAbortKillRetryDelay = oldRetryDelay
	})

	t.Setenv("CC_CODEX_HELPER", "direct")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForThreadID(t, cs, "thread-abort")

	if err := cs.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	// The read loop must exit once the in-flight process group is dead, so
	// Close completes quickly even though the session stays open for resume.
	done := make(chan error, 1)
	go func() { done <- cs.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after Abort: %v", err)
		}
	case <-time.After(3 * time.Second):
		out, _ := exec.Command("ps", "-axo", "pid,ppid,pgid,state,command").CombinedOutput()
		t.Fatalf("Close blocked after Abort; in-flight process was not killed\nprocess tree:\n%s", out)
	}
}

func TestClose_ForceKillsProcessGroupAfterGracefulTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-close\"}'\n" +
		"(sleep 0.12; printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"late child output\"}}'; sleep 30) &\n" +
		"wait\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldCloseTimeout := codexSessionCloseTimeout
	oldForceKillWait := codexSessionForceKillWait
	codexSessionCloseTimeout = 50 * time.Millisecond
	codexSessionForceKillWait = 500 * time.Millisecond
	t.Cleanup(func() {
		codexSessionCloseTimeout = oldCloseTimeout
		codexSessionForceKillWait = oldForceKillWait
	})

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitForThreadID(t, cs, "thread-close")

	closeStarted := time.Now()
	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(closeStarted); elapsed > time.Second {
		t.Fatalf("Close took too long after force kill: %v", elapsed)
	}

	select {
	case evt, ok := <-cs.Events():
		if ok {
			t.Fatalf("unexpected event after Close: %#v", evt)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("timed out waiting for events channel to close")
	}
}

func TestClose_ForceKillsAllTrackedProcessesAfterCmdOverwrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on windows")
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	startsFile := filepath.Join(workDir, "starts.txt")
	script := "#!/bin/sh\n" +
		"prompt=$(cat)\n" +
		"printf '%s\\n' \"$prompt\" >> \"$CODEX_STARTS_FILE\"\n" +
		"if [ \"$prompt\" = \"first\" ]; then\n" +
		"  printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-overlap\"}'\n" +
		"  printf '%s\\n' '{\"type\":\"turn.completed\"}'\n" +
		"fi\n" +
		"sleep 30\n"
	writeFakeCodexScript(t, binDir, script)

	t.Setenv("CODEX_STARTS_FILE", startsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldCloseTimeout := codexSessionCloseTimeout
	oldForceKillWait := codexSessionForceKillWait
	codexSessionCloseTimeout = 50 * time.Millisecond
	codexSessionForceKillWait = 500 * time.Millisecond
	t.Cleanup(func() {
		codexSessionCloseTimeout = oldCloseTimeout
		codexSessionForceKillWait = oldForceKillWait
	})

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "first"}}}); err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	waitForThreadID(t, cs, "thread-overlap")
	waitForDoneResult(t, cs.Events())

	if err := cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "second"}}}); err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	waitForFileLines(t, startsFile, 2)

	closeStarted := time.Now()
	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(closeStarted); elapsed > time.Second {
		t.Fatalf("Close took too long after force killing tracked processes: %v", elapsed)
	}

	select {
	case evt, ok := <-cs.Events():
		if ok {
			t.Fatalf("unexpected event after Close: %#v", evt)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("timed out waiting for events channel to close")
	}
}

func TestSessionSend_AfterClose_ReturnsError(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, t.TempDir(), "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	if err := cs.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = cs.Send(context.Background(), gwrt.Input{Messages: []gwrt.Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Send after Close err = %v, want 'session is closed'", err)
	}
}

// TestContextUsageFromRollout_UsesLastTokenCount reads a pre-written codex
// rollout JSONL (the native file codex writes per thread) and asserts the
// token_count event maps into the canonical runtime.Usage.
func TestContextUsageFromRollout_UsesLastTokenCount(t *testing.T) {
	workDir := t.TempDir()
	codexHome := filepath.Join(workDir, ".codex")
	rolloutDir := filepath.Join(codexHome, "sessions", "2026", "04", "12")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatalf("mkdir rollout dir: %v", err)
	}

	sessionID := "019d8019-d05a-7612-ace2-db549494c0f9"
	rolloutPath := filepath.Join(rolloutDir, "rollout-2026-04-12T05-11-08-"+sessionID+".jsonl")
	rollout := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"/tmp/project"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex"}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50665316,"cached_input_tokens":46971872,"output_tokens":156453,"reasoning_output_tokens":75023,"total_tokens":50821769},"last_token_usage":{"input_tokens":180805,"cached_input_tokens":139776,"output_tokens":619,"reasoning_output_tokens":32,"total_tokens":181424},"model_context_window":258400},"rate_limits":{"limit_id":"codex"}}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", sessionID, "", []string{"CODEX_HOME=" + codexHome}, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	cs.refreshContextUsageFromRollout()

	usage := cs.LastUsage()
	if usage == nil {
		t.Fatal("LastUsage() = nil, want rollout token count")
	}
	if usage.TotalTokens != 181424 {
		t.Fatalf("total tokens = %d, want 181424", usage.TotalTokens)
	}
	if usage.InputTokens != 180805 {
		t.Fatalf("input tokens = %d, want 180805", usage.InputTokens)
	}
	if usage.OutputTokens != 619 {
		t.Fatalf("output tokens = %d, want 619", usage.OutputTokens)
	}
}

// TestPermissionEventPath asserts the canonical permission event surface is
// wired even though the exec backend runs with approval_policy=never and never
// surfaces approvals (auto-approve happens at the worker/config layer, not by
// deleting the event path).
func TestPermissionEventPath(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, t.TempDir(), "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	cs.emitPermission(gwrt.PermissionRequest{ID: "req-1", Action: "run_command", Detail: "rm -rf build"})

	select {
	case evt := <-cs.Events():
		if evt.Type != gwrt.EventPermission {
			t.Fatalf("event type = %s, want %s", evt.Type, gwrt.EventPermission)
		}
		if evt.Permission == nil {
			t.Fatal("permission event missing Permission payload")
		}
		if evt.Permission.ID != "req-1" || evt.Permission.Action != "run_command" || evt.Permission.Detail != "rm -rf build" {
			t.Fatalf("permission = %+v, want req-1/run_command", evt.Permission)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission event")
	}
}

// TestPermissionEvent_ThroughHandleEvent feeds a native approval.requested
// JSONL line through the session's handleEvent path (not emitPermission
// directly) and asserts the canonical EventPermission is emitted with the
// fields codexPermissionEvent extracts (id, request_id fallback, action,
// detail).
func TestPermissionEvent_ThroughHandleEvent(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "codex", nil, t.TempDir(), "", "", "", "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close(context.Background())

	for _, tc := range []struct {
		name string
		line string
		want gwrt.PermissionRequest
	}{
		{
			name: "id",
			line: `{"type":"approval.requested","id":"req-42","action":"run_command","detail":"rm -rf build"}`,
			want: gwrt.PermissionRequest{ID: "req-42", Action: "run_command", Detail: "rm -rf build"},
		},
		{
			name: "request_id fallback",
			line: `{"type":"approval.requested","request_id":"req-7","action":"apply_patch","detail":"src/main.go"}`,
			want: gwrt.PermissionRequest{ID: "req-7", Action: "apply_patch", Detail: "src/main.go"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(tc.line), &raw); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			cs.handleEvent(raw)

			select {
			case evt := <-cs.Events():
				if evt.Type != gwrt.EventPermission {
					t.Fatalf("event type = %s, want %s", evt.Type, gwrt.EventPermission)
				}
				if evt.Permission == nil {
					t.Fatal("permission event missing Permission payload")
				}
				if evt.Permission.ID != tc.want.ID || evt.Permission.Action != tc.want.Action || evt.Permission.Detail != tc.want.Detail {
					t.Fatalf("permission = %+v, want %+v", evt.Permission, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for permission event")
			}
		})
	}
}

func TestDescribe_DeclaresLifecycleAndCapabilities(t *testing.T) {
	a, err := NewAdapter(Options{Command: "codex"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "codex" {
		t.Fatalf("ModelID = %q, want codex", desc.ModelID)
	}
	if desc.LifecycleMode != gwrt.LifecycleResumePerTurn {
		t.Fatalf("LifecycleMode = %q, want %q", desc.LifecycleMode, gwrt.LifecycleResumePerTurn)
	}
	if !desc.Capabilities.Streaming || !desc.Capabilities.ToolCalls || !desc.Capabilities.Reasoning ||
		!desc.Capabilities.Permission || !desc.Capabilities.Resume || !desc.Capabilities.MultiTurn {
		t.Fatalf("Capabilities = %+v, want all capabilities enabled", desc.Capabilities)
	}
}

func TestAdapterStart_ResumeIDFromMetadata(t *testing.T) {
	t.Setenv("GW_WORKSPACE_DIR", t.TempDir())
	a, err := NewAdapter(Options{Command: "codex"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	sess, err := a.Start(context.Background(), gwrt.StartRequest{
		ModelID:     "codex",
		SessionID:   "sess-1",
		CallerID:     "owner-1",
		WorkspaceID: "ws-1",
		Metadata:    map[string]string{"codex_thread_id": "thread-native"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(context.Background())

	cs, ok := sess.(*codexSession)
	if !ok {
		t.Fatalf("session type = %T, want *codexSession", sess)
	}
	if got := cs.CurrentSessionID(); got != "thread-native" {
		t.Fatalf("CurrentSessionID() = %q, want thread-native (native resume from metadata)", got)
	}
}

func TestAdapterStart_RejectsAppServerBackend(t *testing.T) {
	a, err := NewAdapter(Options{Command: "codex", Backend: "app_server"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	_, err = a.Start(context.Background(), gwrt.StartRequest{
		ModelID: "codex", SessionID: "s", CallerID: "o", WorkspaceID: "w",
	})
	if err == nil || !strings.Contains(err.Error(), "app_server") {
		t.Fatalf("Start(app_server) err = %v, want app_server not supported", err)
	}
}

func TestRegistry_RegistersCodex(t *testing.T) {
	names := gwrt.List()
	found := false
	for _, n := range names {
		if n == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gwrt.List() = %v, want it to contain codex", names)
	}
}

func TestFactory_ResolvesAdapter(t *testing.T) {
	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"turn.completed\"}'\n"
	writeFakeCodexScript(t, binDir, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a, err := gwrt.Resolve(context.Background(), "codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "codex" {
		t.Fatalf("ModelID = %q, want codex", desc.ModelID)
	}
}

func TestNew_CommandNotFound(t *testing.T) {
	t.Setenv("CC_GATEWAY_CODEX_COMMAND", "definitely-not-a-real-codex-binary-xyz")
	_, err := New(context.Background(), "codex")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("New with bad command err = %v, want 'not found in PATH'", err)
	}
}

// ---------------------------------------------------------------------------
// Shared fake-codex helpers (ported from the upstream agent/codex tests).
// ---------------------------------------------------------------------------

func writeFakeCodexScript(t *testing.T, dir, shellScript string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake codex shell shim is unix-only in the migrated tests")
	}
	scriptPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(scriptPath, []byte(shellScript), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
}

// fakeCodexAbortHelperSource is compiled (via `go build`) into the fake
// `codex` binary for TestAbort_KillsInFlightProcess. It reproduces the abort
// race that the single-kill pre-fix Abort could not close:
//
//   - "direct" mode — the session's direct child (process-group leader):
//     prints thread.started, spawns the keeper into its OWN process group, then
//     stays in-flight like a long-running turn.
//   - "keeper" mode — inherits the session's stdout pipe write end. Because it
//     lives in a separate process group, the abort's FIRST group SIGKILL
//     deterministically misses it. The keeper polls its parent pid; as soon as
//     the direct child dies (the first kill landed, the keeper is reparented to
//     launchd) it joins the direct child's process group with setpgid, so the
//     post-fix Abort's re-kill loop (re-kill the group until the direct child
//     is reaped) deterministically catches it. The pre-fix single group-kill
//     never catches it: the orphan holds the pipe open, the read loop never
//     sees EOF, the direct child is never reaped, and Close times out.
const fakeCodexAbortHelperSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func main() {
	switch os.Getenv("CC_CODEX_HELPER") {
	case "direct":
		direct()
	case "keeper":
		keeper()
	}
}

func direct() {
	fmt.Printf("{\"type\":\"thread.started\",\"thread_id\":\"thread-abort\"}\n")

	keeper := exec.Command(os.Args[0], "keeper")
	keeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	keeper.Stdout = os.Stdout // inherit the stdout pipe write end
	keeper.Stderr = os.Stderr
	keeper.Env = append(os.Environ(),
		"CC_CODEX_HELPER=keeper",
		"CC_CODEX_JOIN_PGID="+strconv.Itoa(os.Getpid()),
	)
	if err := keeper.Start(); err != nil {
		os.Exit(1)
	}
	time.Sleep(30 * time.Second) // stay in-flight like a long-running turn
}

func keeper() {
	pgid, err := strconv.Atoi(os.Getenv("CC_CODEX_JOIN_PGID"))
	if err != nil {
		os.Exit(1)
	}
	// The keeper's parent is the direct child. Wait for the direct child to
	// die — the abort's first group SIGKILL reparents us to launchd — then
	// join its process group so the post-fix Abort's re-kill loop can catch
	// us. Joining earlier (before the first kill) would just get us killed
	// along with the direct child, which is exactly the pre-fix behavior this
	// test must distinguish.
	for os.Getppid() != 1 {
		time.Sleep(time.Millisecond)
	}
	if err := syscall.Setpgid(0, pgid); err != nil {
		os.Exit(1)
	}
	// Hold the stdout pipe write end open until the re-kill loop catches us.
	time.Sleep(30 * time.Second)
}
`

// buildFakeCodexAbortHelper compiles the fake codex binary used by the abort
// race test. `go build` of a single main file needs no module; if the go
// toolchain is unavailable the test skips (the race cannot be exercised
// without it).
func buildFakeCodexAbortHelper(t *testing.T, binDir string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available for abort-race fake codex: %v", err)
	}
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(srcPath, []byte(fakeCodexAbortHelperSource), 0o644); err != nil {
		t.Fatalf("write helper source: %v", err)
	}
	binPath := filepath.Join(binDir, "codex")
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("go build of fake codex failed: %v\n%s", err, out)
	}
	return binPath
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

func waitForArgsSequence(t *testing.T, path string, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if containsSequence(readArgsFile(t, path), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for args %v in %s", want, path)
}

func waitForFileEquals(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("stdin file %s: got %q, want %q", path, string(data), want)
}

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

func waitForThreadID(t *testing.T, cs *codexSession, want string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case <-time.After(10 * time.Millisecond):
			if cs.CurrentSessionID() == want {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for thread id %q", want)
		}
	}
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

func valueAfter(args []string, key string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			return args[i+1]
		}
	}
	return ""
}

func indexOf(args []string, target string) int {
	for i, arg := range args {
		if arg == target {
			return i
		}
	}
	return -1
}
