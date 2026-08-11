package kimi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHelpFlagsAndBuildArgs(t *testing.T) {
	flags := ParseHelpFlags("Usage: kimi [--print] [--prompt TEXT] [--output-format FORMAT]")
	if !flags.Print {
		t.Fatal("legacy --print flag was not detected")
	}
	opts := NormalizeOptions(Options{WorkDir: "/workspace", Model: "kimi-k2", Mode: "plan", Flags: flags})
	args := BuildArgs(opts, "hello", "session-1")
	for _, want := range []string{"--print", "--output-format", "stream-json", "--plan", "--resume", "session-1", "--model", "kimi-k2", "--work-dir", "/workspace", "--prompt", "hello"} {
		if !containsArg(args, want) {
			t.Fatalf("args missing %q: %v", want, args)
		}
	}
	modern := BuildArgs(NormalizeOptions(Options{}), "hello", "")
	if containsArg(modern, "--print") {
		t.Fatalf("modern conservative args contain --print: %v", modern)
	}
}

func TestProbeFlagsUsesInstalledHelpSurface(t *testing.T) {
	flags := ProbeFlags(context.Background(), []string{"sh", "-c", "printf '%s\\n' 'Usage: kimi --print --prompt TEXT --output-format FORMAT'"}, 10*time.Second)
	if !flags.Print {
		t.Fatal("probe did not detect --print")
	}
	missing := ProbeFlags(context.Background(), []string{"definitely-missing-kimi-binary"}, 20*time.Millisecond)
	if missing.Print {
		t.Fatal("failed probe must use conservative no-print surface")
	}
}

func TestSessionStartsPerTurnAndResumesFromStderrTrailer(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "invocations.jsonl")
	session := newKimiTestSession(t, map[string]string{"KIMI_TEST_LOG": logFile})
	for _, prompt := range []string{"first", "second"} {
		if err := session.Send(context.Background(), Input{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		events := readKimiTurn(t, session.Events())
		text := findKimiEvent(events, EventText)
		if text == nil || text.Text != "echo:"+prompt {
			t.Fatalf("events = %#v", events)
		}
		usage := findKimiEvent(events, EventUsage)
		if usage == nil || usage.Usage.TotalTokens != 5 {
			t.Fatalf("usage = %#v", usage)
		}
	}
	invocations := readKimiLog(t, logFile)
	if len(invocations) != 2 || strings.Contains(invocations[0], "--resume") || !strings.Contains(invocations[1], "--resume kimi-native-1") {
		t.Fatalf("invocations = %#v", invocations)
	}
}

func TestAbortKillsTurnAndSessionRemainsUsable(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	session := newKimiTestSession(t, map[string]string{"KIMI_TEST_READY": ready})
	if err := session.Send(context.Background(), Input{Prompt: "block"}); err != nil {
		t.Fatal(err)
	}
	waitKimiFile(t, ready)
	if err := session.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !session.Alive() {
		t.Fatal("session closed after turn abort")
	}
	if err := session.Send(context.Background(), Input{Prompt: "after"}); err != nil {
		t.Fatal(err)
	}
	if events := readKimiTurn(t, session.Events()); findKimiEvent(events, EventFinish) == nil {
		t.Fatalf("post-abort events = %#v", events)
	}
}

func newKimiTestSession(t *testing.T, extra map[string]string) *Session {
	t.Helper()
	env := []string{"GO_WANT_KIMI_NATIVE_HELPER=1"}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	script := filepath.Join(t.TempDir(), "fake-kimi.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ -n "$KIMI_TEST_LOG" ]; then printf '%s\n' "$*" >> "$KIMI_TEST_LOG"; fi
prompt=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--prompt" ]; then shift; prompt="$1"; fi
  shift
done
if [ "$prompt" = "block" ]; then
  printf ready > "$KIMI_TEST_READY"
  sleep 30
  exit 0
fi
printf '{"role":"assistant","content":[{"type":"text","text":"echo:%s"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}\n' "$prompt"
printf 'To resume this session: kimi -r kimi-native-1\n' >&2
`), 0o700); err != nil {
		t.Fatal(err)
	}
	session := New(Options{Command: []string{"sh", script}, Env: env, WorkDir: t.TempDir(), Timeout: 15 * time.Second})
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

func readKimiTurn(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("events closed: %#v", got)
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

func findKimiEvent(events []Event, kind EventKind) *Event {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

func containsArg(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasKimiSequence(values []string, sequence ...string) bool {
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

func readKimiLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
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
