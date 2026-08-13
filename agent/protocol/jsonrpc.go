package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
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

// OverflowHandler is invoked (once per overflow episode, from the drain
// goroutine) when notifications were dropped because the bounded queue filled.
// dropped is the number of notifications lost since the last episode. A handler
// typically surfaces a display marker so a consumer can tell its stream was
// truncated. It must be cheap or done-guarded: a blocking handler stalls the
// drain (but never the reader loop).
type OverflowHandler func(dropped int)

// JSONRPCClient owns bounded JSONL framing, request correlation, and reverse
// message dispatch for one stdio JSON-RPC connection.
type JSONRPCClient struct {
	stdin  io.WriteCloser
	stdout io.Reader

	reverse         ReverseHandler
	notify          NotificationHandler
	overflow        OverflowHandler
	dropped         atomic.Int64 // notifications dropped to queue overflow
	overflowPending atomic.Bool  // an overflow episode awaits a flush

	// notifyQueue decouples the reader loop from the notification consumer.
	// The reader enqueues NON-blocking: a full queue never stalls the reader,
	// so RPC responses and reverse requests are always correlated/processed.
	// Overflowing notifications are dropped and counted; once the drain catches
	// up (the queue empties) the overflow handler fires once with the dropped
	// count so the consumer can surface a truncation marker. A sync marker item
	// lets a caller wait until notifications enqueued before it are delivered.
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

// notifyQueueSize bounds the decoupled notification buffer. The reader never
// blocks on it: when full, notifications are dropped (and counted) until the
// consumer catches up or the session is torn down. It is sized to absorb
// realistic bursts well beyond a downstream events channel without loss.
const notifyQueueSize = 256

// notifyItem is one entry in the decoupled notification queue: a notification
// to deliver, or a sync marker a caller is waiting on.
type notifyItem struct {
	msg  RPCMessage
	sync chan struct{}
}

// NewJSONRPCClient starts a single reader loop for the supplied stdio pair.
// onOverflow, when non-nil, is called once per overflow episode with the number
// of dropped notifications.
func NewJSONRPCClient(stdin io.WriteCloser, stdout io.Reader, maxFrame int, reverse ReverseHandler, notify NotificationHandler, onOverflow OverflowHandler) *JSONRPCClient {
	c := &JSONRPCClient{
		stdin: stdin, stdout: stdout, reverse: reverse, notify: notify, overflow: onOverflow,
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

// enqueueNotify adds a notification to the drain queue without ever blocking
// the reader. On overflow it drops the notification, counts it, and marks an
// overflow episode; the drain flushes the episode (firing the overflow handler)
// once it has caught up.
func (c *JSONRPCClient) enqueueNotify(msg RPCMessage) {
	select {
	case c.notifyQueue <- notifyItem{msg: msg}:
	default:
		c.dropped.Add(1)
		c.overflowPending.Store(true)
	}
}

// drainNotifications delivers queued notifications to the handler serially,
// preserving order, on a goroutine separate from the reader loop. When the
// queue empties after an overflow episode, it fires the overflow handler once
// with the number dropped so the consumer can surface a truncation marker.
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
			// Once the queue has drained, flush any pending overflow episode.
			if c.overflowPending.Load() && len(c.notifyQueue) == 0 {
				c.flushOverflow()
			}
		case <-c.done:
			return
		}
	}
}

// flushOverflow fires the overflow handler once for the current episode and
// resets the dropped counter. Coalesces many drops into a single notice.
func (c *JSONRPCClient) flushOverflow() {
	if !c.overflowPending.CompareAndSwap(true, false) {
		return
	}
	n := c.dropped.Swap(0)
	if c.overflow != nil && n > 0 {
		c.overflow(int(n))
	}
}

// NotifyDone returns a channel that closes when the notification drain
// goroutine has fully exited, or nil if no notification handler was configured.
// A caller that closes a channel its notify handler sends on should cancel the
// handler's unblock signal first, then wait on this before closing, so the
// drain never sends on a closed channel.
func (c *JSONRPCClient) NotifyDone() <-chan struct{} { return c.notifyDone }

// DroppedNotifications returns the total notifications dropped to queue
// overflow for diagnostics. It does not reset the counter.
func (c *JSONRPCClient) DroppedNotifications() int64 { return c.dropped.Load() }

// Sync blocks until every notification enqueued before this call has been
// delivered to the handler (or the connection terminates / queue is saturated).
// It lets a caller whose effect is triggered by an RPC response — which on the
// wire follows the turn's notifications — defer that effect until the
// notifications have been processed, preserving event ordering when the
// consumer is healthy. Under sustained overflow (a stalled consumer) ordering
// is already broken by drops, so Sync returns without waiting rather than
// block; the consumer is not reading anyway. Returns immediately when no
// notification handler is configured.
func (c *JSONRPCClient) Sync() {
	if c.notifyQueue == nil {
		return
	}
	marker := make(chan struct{})
	select {
	case c.notifyQueue <- notifyItem{sync: marker}:
	default:
		// Queue saturated: notifications are being dropped, so strict ordering
		// cannot be guaranteed — don't block the caller.
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
					c.enqueueNotify(msg)
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
