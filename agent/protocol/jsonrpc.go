package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// RPCMessage is one JSON-RPC 2.0 request, response, or notification frame.
type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the structured error member of a JSON-RPC response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "json-rpc error"
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

// ReverseHandler handles a server-to-client JSON-RPC request.
type ReverseHandler func(context.Context, RPCMessage) (any, *RPCError)

// NotificationHandler handles a server-to-client JSON-RPC notification.
type NotificationHandler func(RPCMessage)

type rpcResult struct {
	message RPCMessage
	err     error
}

// JSONRPCClient owns bounded JSONL framing, request correlation, and reverse
// message dispatch for one stdio JSON-RPC connection.
type JSONRPCClient struct {
	stdin  io.WriteCloser
	stdout io.Reader

	reverse ReverseHandler
	notify  NotificationHandler

	// notifyQueue decouples the reader loop from the notification consumer.
	// Notifications are enqueued by readLoop and delivered serially by a
	// single drain goroutine, so a slow or stalled consumer (e.g. a full
	// downstream events channel) never blocks the reader from correlating
	// RPC responses — including the in-flight turn's own response. A sync
	// marker item lets a caller wait until all notifications enqueued before
	// it have been delivered, preserving event ordering relative to an RPC
	// response read afterwards.
	notifyQueue chan notifyItem
	// notifyDone is closed when the drain goroutine exits. A session that
	// closes a channel its notify handler sends on must wait on NotifyDone()
	// (after cancelling whatever context unblocks the handler) before closing,
	// so the drain never sends on a closed channel.
	notifyDone chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan rpcResult
	err     error
	done    chan struct{}
	close   sync.Once
}

// notifyQueueSize bounds the decoupled notification buffer. It absorbs bursts
// far larger than a downstream events channel; a permanently stalled consumer
// eventually fills it, at which point readLoop blocks until the session's turn
// timeout tears the connection down (readLoop's enqueue is done-guarded).
const notifyQueueSize = 256

// notifyItem is one entry in the decoupled notification queue: either a
// notification to deliver, or a sync marker a caller is waiting on.
type notifyItem struct {
	msg  RPCMessage
	sync chan struct{}
}

// NewJSONRPCClient starts a single reader loop for the supplied stdio pair.
func NewJSONRPCClient(stdin io.WriteCloser, stdout io.Reader, maxFrame int, reverse ReverseHandler, notify NotificationHandler) *JSONRPCClient {
	c := &JSONRPCClient{
		stdin: stdin, stdout: stdout, reverse: reverse, notify: notify,
		pending: make(map[string]chan rpcResult), done: make(chan struct{}),
	}
	if notify != nil {
		c.notifyQueue = make(chan notifyItem, notifyQueueSize)
		c.notifyDone = make(chan struct{})
		go c.drainNotifications()
	}
	go c.readLoop(maxFrame)
	return c
}

// drainNotifications delivers queued notifications to the handler serially,
// preserving order, on a goroutine separate from the reader loop.
func (c *JSONRPCClient) drainNotifications() {
	defer close(c.notifyDone)
	for {
		select {
		case item := <-c.notifyQueue:
			if item.sync != nil {
				close(item.sync)
				continue
			}
			c.notify(item.msg)
		case <-c.done:
			return
		}
	}
}

// NotifyDone returns a channel that closes when the notification drain
// goroutine has fully exited, or nil if no notification handler was configured.
// A caller that closes a channel its notify handler sends on should cancel the
// handler's unblock signal first, then wait on this before closing, so the
// drain never sends on a closed channel.
func (c *JSONRPCClient) NotifyDone() <-chan struct{} { return c.notifyDone }

// Sync blocks until every notification enqueued before this call has been
// delivered to the handler (or the connection terminates). It lets a caller
// whose effect is triggered by an RPC response — which on the wire follows the
// turn's notifications — defer that effect until the notifications have been
// processed, preserving event ordering. Returns immediately when no
// notification handler is configured.
func (c *JSONRPCClient) Sync() {
	if c.notifyQueue == nil {
		return
	}
	marker := make(chan struct{})
	select {
	case c.notifyQueue <- notifyItem{sync: marker}:
	case <-c.done:
		return
	}
	select {
	case <-marker:
	case <-c.done:
	}
}

// Call sends a request and waits for its correlated response.
func (c *JSONRPCClient) Call(ctx context.Context, method string, params, result any) error {
	if ctx == nil {
		return fmt.Errorf("json-rpc call %q: nil context", method)
	}
	paramsRaw, err := marshalOptional(params)
	if err != nil {
		return fmt.Errorf("json-rpc call %q params: %w", method, err)
	}

	c.mu.Lock()
	select {
	case <-c.done:
		err := c.err
		c.mu.Unlock()
		return terminalRPCError(err)
	default:
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatUint(c.nextID, 10))
	key := string(id)
	response := make(chan rpcResult, 1)
	c.pending[key] = response
	c.mu.Unlock()

	if err := c.write(RPCMessage{JSONRPC: "2.0", ID: id, Method: method, Params: paramsRaw}); err != nil {
		c.removePending(key, response)
		return fmt.Errorf("json-rpc call %q write: %w", method, err)
	}

	select {
	case got := <-response:
		if got.err != nil {
			return got.err
		}
		if got.message.Error != nil {
			return got.message.Error
		}
		if result == nil || len(got.message.Result) == 0 || string(got.message.Result) == "null" {
			return nil
		}
		if err := json.Unmarshal(got.message.Result, result); err != nil {
			return fmt.Errorf("json-rpc call %q result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(key, response)
		return ctx.Err()
	case <-c.done:
		c.removePending(key, response)
		return terminalRPCError(c.Err())
	}
}

// Notify sends a JSON-RPC notification.
func (c *JSONRPCClient) Notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		return fmt.Errorf("json-rpc notify %q: nil context", method)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return terminalRPCError(c.Err())
	default:
	}
	paramsRaw, err := marshalOptional(params)
	if err != nil {
		return fmt.Errorf("json-rpc notify %q params: %w", method, err)
	}
	if err := c.write(RPCMessage{JSONRPC: "2.0", Method: method, Params: paramsRaw}); err != nil {
		return fmt.Errorf("json-rpc notify %q write: %w", method, err)
	}
	return nil
}

func (c *JSONRPCClient) readLoop(maxFrame int) {
	decoder := NewJSONLDecoder(c.stdout, maxFrame)
	for {
		var msg RPCMessage
		if err := decoder.Decode(&msg); err != nil {
			c.terminate(err)
			return
		}
		if msg.Method != "" {
			if len(msg.ID) == 0 {
				// Enqueue for the drain goroutine instead of dispatching
				// inline: a blocked notify must never stall the reader and
				// delay RPC responses. The enqueue is done-guarded so
				// teardown always unblocks the reader.
				if c.notify != nil {
					select {
					case c.notifyQueue <- notifyItem{msg: msg}:
					case <-c.done:
						return
					}
				}
				continue
			}
			go c.handleReverse(msg)
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		key := string(msg.ID)
		c.mu.Lock()
		pending := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if pending != nil {
			pending <- rpcResult{message: msg}
		}
	}
}

func (c *JSONRPCClient) handleReverse(msg RPCMessage) {
	var result any
	var rpcErr *RPCError
	if c.reverse == nil {
		rpcErr = &RPCError{Code: -32601, Message: "method not found"}
	} else {
		result, rpcErr = c.reverse(context.Background(), msg)
	}
	response := RPCMessage{JSONRPC: "2.0", ID: msg.ID, Error: rpcErr}
	if rpcErr == nil {
		raw, err := marshalOptional(result)
		if err != nil {
			response.Error = &RPCError{Code: -32603, Message: "failed to encode response"}
		} else {
			response.Result = raw
		}
	}
	_ = c.write(response)
}

func (c *JSONRPCClient) write(msg RPCMessage) error {
	if c == nil || c.stdin == nil {
		return io.ErrClosedPipe
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return terminalRPCError(c.Err())
	default:
	}
	_, err = c.stdin.Write(data)
	return err
}

func (c *JSONRPCClient) removePending(key string, expected chan rpcResult) {
	c.mu.Lock()
	if c.pending[key] == expected {
		delete(c.pending, key)
	}
	c.mu.Unlock()
}

func (c *JSONRPCClient) terminate(err error) {
	c.close.Do(func() {
		if err == nil {
			err = io.ErrClosedPipe
		}
		c.mu.Lock()
		c.err = err
		pending := c.pending
		c.pending = make(map[string]chan rpcResult)
		c.mu.Unlock()
		close(c.done)
		for _, waiter := range pending {
			waiter <- rpcResult{err: err}
		}
	})
}

// Close closes both pipe ends when possible and settles every pending call.
func (c *JSONRPCClient) Close() error {
	if c == nil {
		return nil
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if closer, ok := c.stdout.(io.Closer); ok {
		_ = closer.Close()
	}
	c.terminate(io.ErrClosedPipe)
	return nil
}

// Done closes exactly once when the connection terminates.
func (c *JSONRPCClient) Done() <-chan struct{} { return c.done }

// Err returns the terminal reader/framing error after Done closes.
func (c *JSONRPCClient) Err() error {
	if c == nil {
		return io.ErrClosedPipe
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func marshalOptional(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	return json.Marshal(value)
}

func terminalRPCError(err error) error {
	if err == nil {
		return io.ErrClosedPipe
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return err
}
