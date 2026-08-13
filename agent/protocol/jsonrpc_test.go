package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestJSONRPCCorrelatesOutOfOrderResponses(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil, nil)
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

func TestJSONRPCCanceledCallDoesNotPoisonLaterCalls(t *testing.T) {
	serverRead, clientIn := io.Pipe()
	clientOut, serverWrite := io.Pipe()
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil, nil)
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
	client := NewJSONRPCClient(clientIn, clientOut, 1024, nil, nil)

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
	client := NewJSONRPCClient(clientIn, clientOut, 32, nil, nil)
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
