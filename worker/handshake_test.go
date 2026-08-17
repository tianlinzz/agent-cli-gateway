package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	grt "github.com/tianlinzz/agent-cli-gateway/runtime"
	workerpb "github.com/tianlinzz/agent-cli-gateway/worker/proto"
)

// handshakeStubServer is a fake Worker gRPC server for handshake tests: it
// returns a scripted HealthResponse and accepts StartSession, so the full
// supervisor-side handshake (dial → Health → version gate → StartSession)
// runs against it in-process.
type handshakeStubServer struct {
	workerpb.UnimplementedWorkerServer
	health *workerpb.HealthResponse
}

func (s *handshakeStubServer) Health(context.Context, *workerpb.HealthRequest) (*workerpb.HealthResponse, error) {
	return s.health, nil
}

func (s *handshakeStubServer) StartSession(context.Context, *workerpb.StartSessionRequest) (*workerpb.StartSessionResponse, error) {
	return &workerpb.StartSessionResponse{LifecycleMode: grt.LifecyclePersistentProcess}, nil
}

// serveHandshakeStub serves the stub on a short unix socket path (kernel
// limit ~104 bytes) and returns the socket path.
func serveHandshakeStub(t *testing.T, health *workerpb.HealthResponse) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("gwhs-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "w.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	workerpb.RegisterWorkerServer(srv, &handshakeStubServer{health: health})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return sock
}

// newHandshakeSession builds the minimal workerSession handshake needs: a
// socket path, a reaped signal, and a supervisor for the post-health
// StartSession config lookup.
func newHandshakeSession(socketPath string) *workerSession {
	return &workerSession{
		sup:        &Supervisor{cfg: Config{}},
		req:        grt.StartRequest{ModelID: "m", SessionID: "s-hs", CallerID: "o", WorkspaceID: "w"},
		socketPath: socketPath,
		reaped:     make(chan struct{}),
	}
}

// TestHandshake_ProtocolVersionMismatchRejected is the negative version-
// negotiation test: a worker reporting a different protocol version than the
// supervisor's ProtocolVersion is rejected during the handshake (fail-closed)
// and the session is never started.
func TestHandshake_ProtocolVersionMismatchRejected(t *testing.T) {
	sock := serveHandshakeStub(t, &workerpb.HealthResponse{
		Status:          "ok",
		Version:         "stub-1",
		Pid:             4242,
		ProtocolVersion: ProtocolVersion + 1,
	})
	ws := newHandshakeSession(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := ws.handshake(ctx)
	if err == nil {
		t.Fatal("handshake with mismatched protocol version succeeded; want fail-closed rejection")
	}
	if !strings.Contains(err.Error(), "protocol version mismatch") {
		t.Fatalf("handshake error = %v, want protocol version mismatch", err)
	}
}

// TestHandshake_CapturesRegistrationFields asserts the supervisor consumes
// the worker registration fields from the Health handshake: identity lands on
// the session (WorkerIdentity), and the remaining registration fields travel
// the wire intact.
func TestHandshake_CapturesRegistrationFields(t *testing.T) {
	sock := serveHandshakeStub(t, &workerpb.HealthResponse{
		Status:              "ok",
		Version:             "stub-1",
		Pid:                 4242,
		WorkerId:            "worker-7",
		NodeId:              "node-9",
		ProtocolVersion:     ProtocolVersion,
		Arch:                "arm64",
		SandboxCapabilities: []string{"nsjail"},
		Adapters:            []string{"alpha", "beta"},
	})
	ws := newHandshakeSession(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.handshake(ctx); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	workerID, nodeID := ws.WorkerIdentity()
	if workerID != "worker-7" || nodeID != "node-9" {
		t.Fatalf("WorkerIdentity = (%q, %q), want (%q, %q)", workerID, nodeID, "worker-7", "node-9")
	}
	if _, pid := ws.snapshotClient(); pid != 4242 {
		t.Fatalf("worker pid = %d, want 4242", pid)
	}
}

// TestSupervisor_HandshakePopulatesWorkerIdentity exercises the real worker
// child (testworker) end to end: after StartSession the session's worker
// identity equals the gateway session id (GW_WORKER_SESSION_ID) and the host
// node (os.Hostname()).
func TestSupervisor_HandshakePopulatesWorkerIdentity(t *testing.T) {
	sup := newTestSupervisor(t, nil)
	ws, err := sup.StartSession(context.Background(), testRequest("sess-identity"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	workerID, nodeID := ws.WorkerIdentity()
	if workerID != "sess-identity" {
		t.Fatalf("worker id = %q, want the gateway session id", workerID)
	}
	host, herr := os.Hostname()
	if herr == nil && nodeID != host {
		t.Fatalf("node id = %q, want hostname %q", nodeID, host)
	}
	if nodeID == "" {
		t.Fatal("node id is empty")
	}
}
