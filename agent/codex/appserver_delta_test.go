package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// TestAppServerSession_AgentMessageDelta_StreamsIncrementally verifies that
// item/agentMessage/delta notifications emit EventText immediately (streamed
// live), rather than being buffered until turn completion.
func TestAppServerSession_AgentMessageDelta_StreamsIncrementally(t *testing.T) {
	s := newTestAppServerSession()
	s.currentTurn = "turn-1"

	// Simulate two delta fragments arriving before the item completes.
	for _, fragment := range []string{"hello ", "world"} {
		raw, err := json.Marshal(map[string]any{
			"threadId": "thread-1",
			"turnId":   "turn-1",
			"itemId":   "msg-1",
			"delta":    fragment,
		})
		if err != nil {
			t.Fatalf("marshal delta: %v", err)
		}
		s.handleNotification("item/agentMessage/delta", raw)
	}

	// Each delta should produce an immediate EventText.
	var got string
	for i := 0; i < 2; i++ {
		select {
		case event := <-s.events:
			if event.Type != core.EventText {
				t.Fatalf("event[%d] type = %s, want %s", i, event.Type, core.EventText)
			}
			got += event.Content
		case <-time.After(time.Second):
			t.Fatalf("delta %d did not produce an event", i)
		}
	}
	if got != "hello world" {
		t.Fatalf("streamed text = %q, want %q", got, "hello world")
	}
}

// TestAppServerSession_DeltaThenCompleted_NoDup is the regression test: when
// deltas were streamed for an item, the subsequent item/completed (which
// carries the full text) must NOT re-emit it. This was the double-emit bug
// that would occur if deltas were enabled without dedup.
func TestAppServerSession_DeltaThenCompleted_NoDup(t *testing.T) {
	s := newTestAppServerSession()
	s.currentTurn = "turn-1"

	// 1. Stream a delta for itemId "msg-1".
	deltaRaw, err := json.Marshal(map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"itemId":   "msg-1",
		"delta":    "hello world",
	})
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	s.handleNotification("item/agentMessage/delta", deltaRaw)

	// Drain the delta event.
	select {
	case <-s.events:
	case <-time.After(time.Second):
		t.Fatal("delta did not produce an event")
	}

	// 2. item/completed arrives with the FULL text for the same itemId.
	completedRaw, err := json.Marshal(itemNotification{
		Item: map[string]any{
			"type":  "agentMessage",
			"id":    "msg-1",
			"text":  "hello world",
			"phase": "final_answer",
		},
	})
	if err != nil {
		t.Fatalf("marshal completed: %v", err)
	}
	s.handleNotification("item/completed", completedRaw)

	// 3. completeTurn flushes pendingMsgs — but pendingMsgs should be empty
	// because the message was already streamed. No EventText should be emitted
	// from flushPendingAsText.
	s.completeTurn()

	// The only remaining event should be the EventResult (Done=true), NOT a
	// duplicate EventText.
	select {
	case event := <-s.events:
		if event.Type == core.EventText {
			t.Fatalf("duplicate text emitted from completeTurn: %q", event.Content)
		}
		if event.Type != core.EventResult {
			t.Fatalf("event type = %s, want %s (result)", event.Type, core.EventResult)
		}
	case <-time.After(time.Second):
		t.Fatal("expected result event from completeTurn")
	}
}

// TestAppServerSession_ItemCompletedWithoutDelta_StillBuffers confirms the
// fallback path: when NO deltas were received for an item (e.g. resumed
// history messages), item/completed still buffers into pendingMsgs and
// flushes at turn completion. This protects against the regression of
// accidentally skipping all agentMessage buffering.
func TestAppServerSession_ItemCompletedWithoutDelta_StillBuffers(t *testing.T) {
	s := newTestAppServerSession()
	s.currentTurn = "turn-1"

	// No delta notification — just item/completed directly.
	completedRaw, err := json.Marshal(itemNotification{
		Item: map[string]any{
			"type":  "agentMessage",
			"id":    "msg-no-delta",
			"text":  "resumed message",
			"phase": "final_answer",
		},
	})
	if err != nil {
		t.Fatalf("marshal completed: %v", err)
	}
	s.handleNotification("item/completed", completedRaw)
	s.completeTurn()

	select {
	case event := <-s.events:
		if event.Type != core.EventText {
			t.Fatalf("event type = %s, want %s", event.Type, core.EventText)
		}
		if event.Content != "resumed message" {
			t.Fatalf("content = %q, want %q", event.Content, "resumed message")
		}
	case <-time.After(time.Second):
		t.Fatal("expected buffered text event from completeTurn")
	}
}

// TestAppServerSession_TurnStartedClearsStreamedItems verifies the per-turn
// cleanup of the streamedMsgItems map, preventing unbounded growth.
func TestAppServerSession_TurnStartedClearsStreamedItems(t *testing.T) {
	s := newTestAppServerSession()
	s.currentTurn = "turn-1"

	// Record a streamed item.
	s.stateMu.Lock()
	s.streamedMsgItems["msg-old"] = true
	s.stateMu.Unlock()

	// A new turn starts — should clear the map.
	turnStartedRaw, err := json.Marshal(map[string]any{
		"threadId": "thread-1",
		"turn":     map[string]any{"id": "turn-2"},
	})
	if err != nil {
		t.Fatalf("marshal turn started: %v", err)
	}
	s.handleNotification("turn/started", turnStartedRaw)

	s.stateMu.Lock()
	_, stillPresent := s.streamedMsgItems["msg-old"]
	s.stateMu.Unlock()
	if stillPresent {
		t.Fatal("streamedMsgItems was not cleared on turn/started")
	}
}

// newTestAppServerSession constructs an appServerSession with the fields needed
// for notification-handling tests, including the delta-tracking map.
func newTestAppServerSession() *appServerSession {
	return &appServerSession{
		events:          make(chan core.Event, 16),
		streamedMsgItems: make(map[string]bool),
	}
}
