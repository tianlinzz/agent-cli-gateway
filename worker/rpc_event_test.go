package worker

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	workerpb "github.com/tianlinzz/agent-cli-gateway/worker/proto"
)

func TestReasoningEventFrameRoundTrip(t *testing.T) {
	want := runtime.Event{
		Type:      runtime.EventReasoning,
		Reasoning: &runtime.Reasoning{ID: "reason-1", Text: "checking workspace"},
	}
	frame, err := toFrame(want)
	if err != nil {
		t.Fatal(err)
	}
	got := fromFrame(frame)
	if got.Type != runtime.EventReasoning || got.Reasoning == nil {
		t.Fatalf("round trip = %#v", got)
	}
	if got.Reasoning.ID != want.Reasoning.ID || got.Reasoning.Text != want.Reasoning.Text {
		t.Fatalf("reasoning = %#v, want %#v", got.Reasoning, want.Reasoning)
	}
}

// TestEventFrameCanonicalFieldLimits is the O-B2 regression: each variable-
// length event field is bounded by the canonical limit. At/under the limit it
// passes through unchanged; over the limit it is truncated with a marker and
// stays UTF-8 valid — never reaching the gRPC boundary that would abort the
// stream. Covers text, error, tool result, reasoning; boundary, over-limit,
// and an extreme (>> gRPC default) value.
func TestEventFrameCanonicalFieldLimits(t *testing.T) {
	cases := []struct {
		name string
		ev   runtime.Event
		get  func(*runtime.Event) string
	}{
		{"text", runtime.Event{Type: runtime.EventText, Text: strings.Repeat("x", canonicalEventFieldLimit+1)},
			func(e *runtime.Event) string { return e.Text }},
		{"error", runtime.Event{Type: runtime.EventError, Error: strings.Repeat("e", canonicalEventFieldLimit+1)},
			func(e *runtime.Event) string { return e.Error }},
		{"tool_result", runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{ID: "t1", Result: strings.Repeat("r", canonicalEventFieldLimit+1)}},
			func(e *runtime.Event) string { return e.Tool.Result }},
		{"reasoning", runtime.Event{Type: runtime.EventReasoning, Reasoning: &runtime.Reasoning{Text: strings.Repeat("z", canonicalEventFieldLimit+1)}},
			func(e *runtime.Event) string { return e.Reasoning.Text }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			frame, err := toFrame(c.ev)
			if err != nil {
				t.Fatalf("toFrame: %v", err)
			}
			got := fromFrame(frame)
			s := c.get(&got)
			if len(s) > canonicalEventFieldLimit {
				t.Fatalf("field not bounded: %d > %d", len(s), canonicalEventFieldLimit)
			}
			if !strings.HasSuffix(s, eventFieldTruncated) {
				t.Fatalf("over-limit field missing truncation marker (len %d)", len(s))
			}
			if !utf8.ValidString(s) {
				t.Fatalf("truncated field not valid UTF-8")
			}
		})
	}

	// At-limit passes through unchanged (no marker).
	atLimit := strings.Repeat("a", canonicalEventFieldLimit)
	got := fromFrame(mustToFrame(t, runtime.Event{Type: runtime.EventText, Text: atLimit}))
	if got.Text != atLimit {
		t.Fatalf("at-limit text was altered: %d vs %d", len(got.Text), len(atLimit))
	}

	// Extreme value (>> 4 MiB gRPC default) is truncated, not dropped.
	extreme := strings.Repeat("Q", 8*1024*1024)
	got = fromFrame(mustToFrame(t, runtime.Event{Type: runtime.EventText, Text: extreme}))
	if len(got.Text) > canonicalEventFieldLimit || !strings.HasSuffix(got.Text, eventFieldTruncated) {
		t.Fatalf("extreme text not truncated: len %d", len(got.Text))
	}
}

// TestEventFrameUTF8Truncation lands the cut on a rune boundary.
func TestEventFrameUTF8Truncation(t *testing.T) {
	// Build a string of 2-byte runes that overflows the limit, forcing a mid-
	// rune cut; the result must be valid UTF-8.
	s := strings.Repeat("é", (canonicalEventFieldLimit/2)+50)
	got := fromFrame(mustToFrame(t, runtime.Event{Type: runtime.EventText, Text: s}))
	if !utf8.ValidString(got.Text) {
		t.Fatalf("truncated text not valid UTF-8")
	}
	if len(got.Text) > canonicalEventFieldLimit {
		t.Fatalf("truncated text exceeds limit: %d", len(got.Text))
	}
}

// TestEventFrameConcurrentToFromFrame exercises concurrent framing.
func TestEventFrameConcurrentToFromFrame(t *testing.T) {
	ev := runtime.Event{Type: runtime.EventText, Text: strings.Repeat("c", 64)}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = fromFrame(mustToFrame(t, ev))
			}
		}()
	}
	wg.Wait()
}

func mustToFrame(t *testing.T, ev runtime.Event) *workerpb.EventFrame {
	t.Helper()
	f, err := toFrame(ev)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
