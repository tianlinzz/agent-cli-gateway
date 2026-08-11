package worker

import (
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
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
