package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestJSONRPCCorrelatesOutOfOrderResponses(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil, nil, nil, nil)
	t.Cleanup(func() { _ = client.Close() })

	type result struct {
		Value string `json:"value"`
	}
	results := make(chan result, 2)
	errs := make(chan error, 2)
	for _, value := range []string{"first", "second"} {
		value := value
		go func() {
			var got result
			err := client.Call(context.Background(), "echo", map[string]string{"value": value}, &got)
			results <- got
			errs <- err
		}()
	}

	requests := readRPCMessages(t, serverRead, 2)
	for i := len(requests) - 1; i >= 0; i-- {
		var params result
		if err := json.Unmarshal(requests[i].Params, &params); err != nil {
			t.Fatal(err)
		}
		writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: requests[i].ID, Result: mustRaw(t, params)})
	}

	seen := map[string]bool{}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		seen[(<-results).Value] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("results = %#v", seen)
	}
}

func TestJSONRPCDispatchesNotificationAndReverseRequest(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	notifications := make(chan RPCMessage, 1)
	reverseCalls := make(chan RPCMessage, 1)
	client := NewJSONRPCClient(clientIn, clientOut, 1024,
		func(_ context.Context, msg RPCMessage) (any, *RPCError) {
			reverseCalls <- msg
			return map[string]string{"decision": "accept"}, nil
		},
		func(msg RPCMessage) { notifications <- msg },
		nil,
		nil,
	)
	t.Cleanup(func() { _ = client.Close() })

	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", Method: "turn/started", Params: mustRaw(t, map[string]string{"id": "turn-1"})})
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: json.RawMessage(`"approval-1"`), Method: "command/requestApproval", Params: mustRaw(t, map[string]string{"command": "pwd"})})

	select {
	case got := <-notifications:
		if got.Method != "turn/started" {
			t.Fatalf("notification = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
	select {
	case got := <-reverseCalls:
		if got.Method != "command/requestApproval" {
			t.Fatalf("reverse request = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reverse request not delivered")
	}

	response := readRPCMessages(t, serverRead, 1)[0]
	if string(response.ID) != `"approval-1"` || response.Error != nil {
		t.Fatalf("response = %#v", response)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Result, &body); err != nil {
		t.Fatal(err)
	}
	if body["decision"] != "accept" {
		t.Fatalf("response body = %#v", body)
	}
}

// TestJSONRPCStalledNotifyDoesNotBlockResponses is a regression test for O-A2:
// the reader loop used to dispatch notifications inline, so a stalled consumer
// (e.g. a full downstream events channel) blocked the reader and stalled every
// RPC response, including the in-flight turn's own response. Notifications are
// now drained on a separate goroutine, so a stalled notify must not block
// response correlation.
func TestJSONRPCStalledNotifyDoesNotBlockResponses(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	proceed := make(chan struct{})          // held open to stall the notify consumer
	notifyEntered := make(chan struct{}, 1) // signaled once notify is stalled
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil,
		func(RPCMessage) {
			select {
			case notifyEntered <- struct{}{}:
			default:
			}
			<-proceed // simulate a stalled consumer
		},
		nil,
		nil,
	)
	t.Cleanup(func() {
		close(proceed) // unblock the stalled notify so the drain goroutine exits
		_ = client.Close()
	})

	// 1. A notification that stalls the drain goroutine inside notify.
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", Method: "turn/started"})
	select {
	case <-notifyEntered:
	case <-time.After(time.Second):
		t.Fatal("notify never entered (drain goroutine not running)")
	}

	// 2. While notify is stalled, issue a Call and deliver its response.
	callDone := make(chan error, 1)
	go func() { callDone <- client.Call(context.Background(), "turn/start", nil, nil) }()
	time.Sleep(30 * time.Millisecond) // let the Call write its request
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)})
	_ = readRPCMessages(t, serverRead, 1) // drain the request the Call wrote

	// 3. The Call must complete despite notify being stalled.
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("Call returned error while notify stalled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not complete while notify was stalled — reader loop was blocked by notify")
	}
}

// TestJSONRPCOverflowNeverBlocksReader is the O-A2 overflow regression: with
// the notification consumer permanently stalled and the native CLI flooding far
// past the 256-deep queue, the reader loop must keep correlating RPC responses
// (and dispatching reverse requests), drop the overflow, and surface a
// truncation marker once the consumer resumes — never blocking the reader.
func TestJSONRPCOverflowNeverBlocksReader(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	releaseDrain := func() { proceedOnce.Do(func() { close(proceed) }) }
	var overflowSeen atomic.Int64
	notifyEntered := make(chan struct{}, 1)
	reverseDone := make(chan RPCMessage, 1)
	client := NewJSONRPCClient(clientIn, clientOut, 1024,
		func(_ context.Context, msg RPCMessage) (any, *RPCError) {
			select {
			case reverseDone <- msg:
			default:
			}
			return map[string]string{"ok": "1"}, nil
		},
		func(RPCMessage) {
			select {
			case notifyEntered <- struct{}{}:
			default:
			}
			<-proceed // stall the drain
		},
		func(dropped int) { overflowSeen.Add(int64(dropped)) },
		nil,
	)
	t.Cleanup(func() { releaseDrain(); _ = client.Close() })

	// Flood notifications far past the queue while the drain is stalled on the
	// first one. The reader decodes and drops the overflow; it never blocks.
	const flood = 600
	for i := 0; i < flood; i++ {
		writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", Method: "session/update"})
	}
	select {
	case <-notifyEntered:
	case <-time.After(time.Second):
		t.Fatal("drain never entered notify")
	}

	// 1. While the consumer is stalled and overflow has occurred, a correlated
	// RPC response must still be read and delivered.
	callErr := make(chan error, 1)
	go func() { callErr <- client.Call(context.Background(), "session/prompt", nil, nil) }()
	time.Sleep(30 * time.Millisecond) // let the request be written
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)})
	_ = readRPCMessages(t, serverRead, 1)
	select {
	case err := <-callErr:
		if err != nil {
			t.Fatalf("Call under overflow returned %v (reader was blocked)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not complete under overflow — reader blocked by stalled notify")
	}

	// 2. A reverse request is dispatched on its own goroutine and processed.
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: json.RawMessage(`"r1"`), Method: "approval"})
	select {
	case <-reverseDone:
	case <-time.After(time.Second):
		t.Fatal("reverse request not processed under overflow")
	}

	// 3. Overflow was dropped and counted; it stays > 0 while stalled.
	if got := client.DroppedNotifications(); got <= 0 {
		t.Fatalf("DroppedNotifications = %d, want > 0 after flood", got)
	}

	// 4. Release the stalled consumer: the drain flushes and the truncation
	// marker fires the overflow handler exactly once with the dropped count.
	releaseDrain()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && overflowSeen.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if overflowSeen.Load() <= 0 {
		t.Fatal("overflow handler never fired after drain resumed")
	}
}

// TestJSONRPCOverflowNeverDropsCriticalNotifications is the O-A2 control-
// notification reliability test the final review required: with the display
// consumer permanently stalled and the display queue overflowing, critical
// notifications (turn/completed, thread/tokenUsage/updated) must still be
// delivered — the turn finishes and usage is recorded, never lost to overflow.
func TestJSONRPCOverflowNeverDropsCriticalNotifications(t *testing.T) {
	_, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	_ = clientOut
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	releaseDrain := func() { proceedOnce.Do(func() { close(proceed) }) }
	var deliveredCritical atomic.Int64
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil,
		func(msg RPCMessage) {
			if isCriticalTestNotification(msg) {
				deliveredCritical.Add(1)
				return // critical notifications are never stalled
			}
			<-proceed // stall the display consumer
		},
		nil,
		isCriticalTestNotification,
	)
	t.Cleanup(func() { releaseDrain(); _ = client.Close() })

	// Flood display notifications to fill the queue, then send critical ones.
	for i := 0; i < 400; i++ {
		writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", Method: "item/agentMessage/delta"})
	}
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", Method: "turn/completed"})
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", Method: "thread/tokenUsage/updated"})

	// Critical notifications must be delivered despite the stalled display
	// consumer and the overflowing display queue. They go through the blocking
	// critical queue, so the reader pauses until the drain processes them.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && deliveredCritical.Load() < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := deliveredCritical.Load(); got != 2 {
		t.Fatalf("critical notifications delivered = %d, want 2 (turn/completed + usage) — dropped to overflow", got)
	}
	// Display overflow was counted.
	if client.DroppedNotifications() <= 0 {
		t.Fatal("display overflow not counted (expected display notifications dropped)")
	}
}

func isCriticalTestNotification(message RPCMessage) bool {
	method := message.Method
	return method == "turn/completed" || method == "thread/tokenUsage/updated"
}

func TestJSONRPCCanceledCallDoesNotPoisonLaterCalls(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil, nil, nil, nil)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Call(ctx, "slow", nil, nil) }()
	first := readRPCMessages(t, serverRead, 1)[0]
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call error = %v", err)
	}
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: first.ID, Result: json.RawMessage(`{}`)})

	go func() { done <- client.Call(context.Background(), "next", nil, nil) }()
	second := readRPCMessages(t, serverRead, 1)[0]
	writeRPCMessage(t, serverWrite, RPCMessage{JSONRPC: "2.0", ID: second.ID, Result: json.RawMessage(`{}`)})
	if err := <-done; err != nil {
		t.Fatalf("later call: %v", err)
	}
}

func TestJSONRPCEOFFailsAllPendingCallsAndClosesOnce(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil, nil, nil, nil)

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- client.Call(context.Background(), "wait", nil, nil) }()
	}
	_ = readRPCMessages(t, serverRead, 2)
	_ = serverWrite.Close()

	for range 2 {
		if err := <-errs; !errors.Is(err, io.EOF) {
			t.Fatalf("pending call error = %v, want EOF", err)
		}
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("client did not close after EOF")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONRPCRejectsOversizedFrame(t *testing.T) {
	_, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	client := NewJSONRPCClient(clientIn, clientOut, 32, nil, nil, nil, nil)
	go func() {
		_, _ = io.WriteString(serverWrite, `{"jsonrpc":"2.0","method":"`+string(make([]byte, 128))+`"}`+"\n")
		_ = serverWrite.Close()
	}()
	<-client.Done()
	var frameErr *FrameError
	if !errors.As(client.Err(), &frameErr) || frameErr.Kind != ErrFrameTooLarge {
		t.Fatalf("client error = %T %v", client.Err(), client.Err())
	}
}

func readRPCMessages(t *testing.T, reader io.Reader, count int) []RPCMessage {
	t.Helper()
	decoder := json.NewDecoder(bufio.NewReader(reader))
	messages := make([]RPCMessage, 0, count)
	for len(messages) < count {
		var msg RPCMessage
		if err := decoder.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, msg)
	}
	return messages
}

var rpcWriteMu sync.Mutex

func writeRPCMessage(t *testing.T, writer io.Writer, msg RPCMessage) {
	t.Helper()
	rpcWriteMu.Lock()
	defer rpcWriteMu.Unlock()
	if err := json.NewEncoder(writer).Encode(msg); err != nil {
		t.Fatal(err)
	}
}

func mustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
