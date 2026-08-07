// Package worker implements the worker layer of the agent gateway: the
// per-session gRPC contract over Unix sockets, the supervisor that forks and
// reaps one nsjail-wrapped worker per session, and the LocalExecutionBackend
// that is the API layer's only way to execute a model request.
//
// Transport is a worker-layer concern. The runtime package (and therefore the
// API layer) never depends on sockets or gRPC; everything behind
// runtime.ExecutionBackend is hidden here, behind the Endpoint abstraction.
package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	workerpb "github.com/tianlinzz/agent-cli-gateway/worker/proto"
)

// Endpoint abstracts where a worker gRPC server listens. Phase 1 uses a
// per-session Unix socket; a future cross-node deployment swaps in a TCP
// endpoint without touching the API layer or the runtime contract.
type Endpoint interface {
	// Listen creates a listener the gRPC server serves on.
	Listen() (net.Listener, error)
	// DialContext connects a client to the endpoint.
	DialContext(ctx context.Context) (net.Conn, error)
}

// UnixSocket returns an Endpoint backed by a single Unix socket file.
func UnixSocket(path string) Endpoint {
	return unixSocket{path: path}
}

type unixSocket struct {
	path string
}

func (u unixSocket) Listen() (net.Listener, error) {
	lis, err := net.Listen("unix", u.path)
	if err != nil {
		return nil, fmt.Errorf("worker: listen on unix socket %q: %w", u.path, err)
	}
	return lis, nil
}

func (u unixSocket) DialContext(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", u.path)
	if err != nil {
		return nil, fmt.Errorf("worker: dial unix socket %q: %w", u.path, err)
	}
	return conn, nil
}

// StartSessionReq is the worker-layer start request. It mirrors
// runtime.StartRequest but stays in the worker package so the transport layer
// owns its own request shape.
type StartSessionReq struct {
	ModelID     string
	SessionID   string
	OwnerID     string
	WorkspaceID string
	Metadata    map[string]string
	FirstInput  *runtime.Input
}

// ---------------------------------------------------------------------------
// Client side (used by the supervisor in the API process)
// ---------------------------------------------------------------------------

// client is a thin wrapper around the generated gRPC client that speaks
// canonical runtime types. It is bound to a single worker session.
type client struct {
	conn *grpc.ClientConn
	c    workerpb.WorkerClient
}

// dial connects a gRPC client to the worker endpoint. Connection is lazy;
// the first RPC triggers the actual connect. The caller must call Close.
func dial(ctx context.Context, ep Endpoint) (*client, error) {
	conn, err := grpc.NewClient(
		"passthrough:///worker",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ep.DialContext(ctx)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("worker: grpc dial: %w", err)
	}
	return &client{conn: conn, c: workerpb.NewWorkerClient(conn)}, nil
}

// Close closes the underlying gRPC connection.
func (c *client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Health performs the handshake and returns the worker version and PID.
func (c *client) Health(ctx context.Context) (version string, pid int32, err error) {
	resp, err := c.c.Health(ctx, &workerpb.HealthRequest{})
	if err != nil {
		return "", 0, err
	}
	return resp.Version, resp.Pid, nil
}

// StartSession starts the session and returns the adapter's lifecycle mode.
func (c *client) StartSession(ctx context.Context, req StartSessionReq) (string, error) {
	p, err := toProtoStartRequest(req)
	if err != nil {
		return "", err
	}
	resp, err := c.c.StartSession(ctx, p)
	if err != nil {
		return "", err
	}
	return resp.LifecycleMode, nil
}

// SendInput delivers a turn to the worker.
func (c *client) SendInput(ctx context.Context, sessionID string, input runtime.Input) error {
	p, err := toProtoInput(input)
	if err != nil {
		return err
	}
	_, err = c.c.SendInput(ctx, &workerpb.SendInputRequest{SessionId: sessionID, Input: p})
	return err
}

// StreamEvents opens the canonical event stream for the session.
func (c *client) StreamEvents(ctx context.Context, sessionID string) (workerpb.Worker_StreamEventsClient, error) {
	return c.c.StreamEvents(ctx, &workerpb.StreamEventsRequest{SessionId: sessionID})
}

// Abort cancels the in-flight turn.
func (c *client) Abort(ctx context.Context, sessionID string) error {
	_, err := c.c.AbortSession(ctx, &workerpb.AbortSessionRequest{SessionId: sessionID})
	return err
}

// CloseSession tears the session down inside the worker.
func (c *client) CloseSession(ctx context.Context, sessionID string) error {
	_, err := c.c.CloseSession(ctx, &workerpb.CloseSessionRequest{SessionId: sessionID})
	return err
}

// ---------------------------------------------------------------------------
// Server side (implemented by the worker process)
// ---------------------------------------------------------------------------

// Handler is the session contract a worker process implements. The worker
// child (task 4 adapters, or the test stub) wires its runtime.Session into a
// Handler and calls Serve. One handler serves exactly one session.
type Handler interface {
	// Health reports worker liveness and version.
	Health(ctx context.Context) (version string, err error)
	// StartSession begins the session and returns the adapter's lifecycle mode
	// (runtime.LifecyclePersistentProcess or runtime.LifecycleResumePerTurn).
	StartSession(ctx context.Context, req StartSessionReq) (lifecycleMode string, err error)
	// SendInput delivers a turn.
	SendInput(ctx context.Context, input runtime.Input) error
	// Events returns the canonical event stream. The handler owns this channel
	// and closes it when the session ends (the stream then terminates).
	Events() <-chan runtime.Event
	// Abort cancels the in-flight turn.
	Abort(ctx context.Context) error
	// Close tears the session down. The worker process then exits.
	Close(ctx context.Context) error
}

// Serve runs the worker gRPC server on the endpoint until ctx is cancelled
// (the worker is shutting down) or the server fails. It is the worker-side
// entrypoint used by the worker child binary.
func Serve(ctx context.Context, ep Endpoint, h Handler) error {
	lis, err := ep.Listen()
	if err != nil {
		return fmt.Errorf("worker: serve: %w", err)
	}
	srv := grpc.NewServer()
	workerpb.RegisterWorkerServer(srv, &workerServer{h: h})

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	select {
	case err := <-serveErr:
		if err == nil {
			return nil
		}
		return fmt.Errorf("worker: serve: %w", err)
	case <-ctx.Done():
		// Drain in-flight RPCs briefly, then force stop. The supervisor kills
		// the process group anyway on teardown.
		stopped := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			srv.Stop()
		}
		return nil
	}
}

// workerServer adapts a Handler to the generated Worker gRPC service.
type workerServer struct {
	workerpb.UnimplementedWorkerServer
	h Handler
}

func (s *workerServer) Health(ctx context.Context, _ *workerpb.HealthRequest) (*workerpb.HealthResponse, error) {
	version, err := s.h.Health(ctx)
	if err != nil {
		return nil, err
	}
	return &workerpb.HealthResponse{
		Status:  "ok",
		Version: version,
		Pid:     int32(os.Getpid()),
	}, nil
}

func (s *workerServer) StartSession(ctx context.Context, req *workerpb.StartSessionRequest) (*workerpb.StartSessionResponse, error) {
	r, err := fromProtoStartRequest(req)
	if err != nil {
		return nil, err
	}
	mode, err := s.h.StartSession(ctx, r)
	if err != nil {
		return nil, err
	}
	return &workerpb.StartSessionResponse{LifecycleMode: mode}, nil
}

func (s *workerServer) SendInput(ctx context.Context, req *workerpb.SendInputRequest) (*workerpb.SendInputResponse, error) {
	in, err := fromProtoInput(req.Input)
	if err != nil {
		return nil, err
	}
	if err := s.h.SendInput(ctx, in); err != nil {
		return nil, err
	}
	return &workerpb.SendInputResponse{}, nil
}

func (s *workerServer) StreamEvents(_ *workerpb.StreamEventsRequest, stream workerpb.Worker_StreamEventsServer) error {
	for {
		select {
		case ev, ok := <-s.h.Events():
			if !ok {
				return nil // session ended; the stream closes cleanly
			}
			frame, err := toFrame(ev)
			if err != nil {
				return fmt.Errorf("worker: encode event: %w", err)
			}
			if err := stream.Send(frame); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func (s *workerServer) AbortSession(ctx context.Context, _ *workerpb.AbortSessionRequest) (*workerpb.AbortSessionResponse, error) {
	if err := s.h.Abort(ctx); err != nil {
		return nil, err
	}
	return &workerpb.AbortSessionResponse{}, nil
}

func (s *workerServer) CloseSession(ctx context.Context, _ *workerpb.CloseSessionRequest) (*workerpb.CloseSessionResponse, error) {
	if err := s.h.Close(ctx); err != nil {
		return nil, err
	}
	return &workerpb.CloseSessionResponse{}, nil
}

// ---------------------------------------------------------------------------
// Canonical <-> proto conversion. This is the ONLY place the wire format is
// translated; everything above and below speaks canonical runtime types.
// ---------------------------------------------------------------------------

func toProtoStartRequest(req StartSessionReq) (*workerpb.StartSessionRequest, error) {
	p := &workerpb.StartSessionRequest{
		ModelId:     req.ModelID,
		SessionId:   req.SessionID,
		OwnerId:     req.OwnerID,
		WorkspaceId: req.WorkspaceID,
		Metadata:    req.Metadata,
	}
	if req.FirstInput != nil {
		in, err := toProtoInput(*req.FirstInput)
		if err != nil {
			return nil, err
		}
		p.FirstInput = in
	}
	return p, nil
}

func fromProtoStartRequest(p *workerpb.StartSessionRequest) (StartSessionReq, error) {
	req := StartSessionReq{
		ModelID:     p.ModelId,
		SessionID:   p.SessionId,
		OwnerID:     p.OwnerId,
		WorkspaceID: p.WorkspaceId,
		Metadata:    p.Metadata,
	}
	if p.FirstInput != nil {
		in, err := fromProtoInput(p.FirstInput)
		if err != nil {
			return StartSessionReq{}, err
		}
		req.FirstInput = &in
	}
	return req, nil
}

func toProtoInput(in runtime.Input) (*workerpb.Input, error) {
	out := &workerpb.Input{Metadata: in.Metadata}
	for _, m := range in.Messages {
		pm, err := toProtoMessage(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, pm)
	}
	for _, t := range in.Tools {
		pt, err := toProtoTool(t)
		if err != nil {
			return nil, err
		}
		out.Tools = append(out.Tools, pt)
	}
	return out, nil
}

func fromProtoInput(p *workerpb.Input) (runtime.Input, error) {
	if p == nil {
		return runtime.Input{}, nil
	}
	out := runtime.Input{Metadata: p.Metadata}
	for _, pm := range p.Messages {
		m, err := fromProtoMessage(pm)
		if err != nil {
			return runtime.Input{}, err
		}
		out.Messages = append(out.Messages, m)
	}
	for _, pt := range p.Tools {
		t, err := fromProtoTool(pt)
		if err != nil {
			return runtime.Input{}, err
		}
		out.Tools = append(out.Tools, t)
	}
	return out, nil
}

func toProtoMessage(m runtime.Message) (*workerpb.Message, error) {
	pm := &workerpb.Message{
		Role:       m.Role,
		Content:    m.Content,
		Name:       m.Name,
		ToolCallId: m.ToolCallID,
	}
	for _, tc := range m.ToolCalls {
		ptc, err := toProtoToolCall(tc)
		if err != nil {
			return nil, err
		}
		pm.ToolCalls = append(pm.ToolCalls, ptc)
	}
	return pm, nil
}

func fromProtoMessage(pm *workerpb.Message) (runtime.Message, error) {
	m := runtime.Message{
		Role:       pm.Role,
		Content:    pm.Content,
		Name:       pm.Name,
		ToolCallID: pm.ToolCallId,
	}
	for _, ptc := range pm.ToolCalls {
		tc, err := fromProtoToolCall(ptc)
		if err != nil {
			return runtime.Message{}, err
		}
		m.ToolCalls = append(m.ToolCalls, tc)
	}
	return m, nil
}

func toProtoTool(t runtime.Tool) (*workerpb.Tool, error) {
	params, err := structValue(t.Parameters)
	if err != nil {
		return nil, fmt.Errorf("worker: tool %q parameters: %w", t.Name, err)
	}
	return &workerpb.Tool{Name: t.Name, Description: t.Description, Parameters: params}, nil
}

func fromProtoTool(pt *workerpb.Tool) (runtime.Tool, error) {
	t := runtime.Tool{Name: pt.Name, Description: pt.Description}
	if pt.Parameters != nil {
		t.Parameters = pt.Parameters.AsMap()
	}
	return t, nil
}

func toProtoToolCall(tc runtime.ToolCall) (*workerpb.ToolCall, error) {
	args, err := structValue(tc.Arguments)
	if err != nil {
		return nil, fmt.Errorf("worker: tool call %q arguments: %w", tc.ID, err)
	}
	return &workerpb.ToolCall{
		Id:        tc.ID,
		Name:      tc.Name,
		Arguments: args,
		Result:    tc.Result,
		IsError:   tc.IsError,
	}, nil
}

func fromProtoToolCall(ptc *workerpb.ToolCall) (runtime.ToolCall, error) {
	tc := runtime.ToolCall{
		ID:      ptc.Id,
		Name:    ptc.Name,
		Result:  ptc.Result,
		IsError: ptc.IsError,
	}
	if ptc.Arguments != nil {
		tc.Arguments = ptc.Arguments.AsMap()
	}
	return tc, nil
}

func toFrame(ev runtime.Event) (*workerpb.EventFrame, error) {
	f := &workerpb.EventFrame{
		Type:         string(ev.Type),
		Text:         ev.Text,
		Error:        ev.Error,
		FinishReason: ev.FinishReason,
		Status:       ev.Status,
	}
	var err error
	if ev.Tool != nil {
		if f.Tool, err = toProtoToolCall(*ev.Tool); err != nil {
			return nil, err
		}
	}
	if ev.Permission != nil {
		f.Permission = &workerpb.PermissionRequest{
			Id:     ev.Permission.ID,
			Action: ev.Permission.Action,
			Detail: ev.Permission.Detail,
		}
	}
	if ev.Usage != nil {
		f.Usage = &workerpb.Usage{
			InputTokens:  int32(ev.Usage.InputTokens),
			OutputTokens: int32(ev.Usage.OutputTokens),
			TotalTokens:  int32(ev.Usage.TotalTokens),
		}
	}
	return f, nil
}

func fromFrame(f *workerpb.EventFrame) runtime.Event {
	ev := runtime.Event{
		Type:         runtime.EventType(f.Type),
		Text:         f.Text,
		Error:        f.Error,
		FinishReason: f.FinishReason,
		Status:       f.Status,
	}
	if f.Tool != nil {
		tc := runtime.ToolCall{
			ID:      f.Tool.Id,
			Name:    f.Tool.Name,
			Result:  f.Tool.Result,
			IsError: f.Tool.IsError,
		}
		if f.Tool.Arguments != nil {
			tc.Arguments = f.Tool.Arguments.AsMap()
		}
		ev.Tool = &tc
	}
	if f.Permission != nil {
		ev.Permission = &runtime.PermissionRequest{
			ID:     f.Permission.Id,
			Action: f.Permission.Action,
			Detail: f.Permission.Detail,
		}
	}
	if f.Usage != nil {
		ev.Usage = &runtime.Usage{
			InputTokens:  int(f.Usage.InputTokens),
			OutputTokens: int(f.Usage.OutputTokens),
			TotalTokens:  int(f.Usage.TotalTokens),
		}
	}
	return ev
}

func structValue(m map[string]any) (*structpb.Struct, error) {
	if m == nil {
		return nil, nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, err
	}
	return s, nil
}
