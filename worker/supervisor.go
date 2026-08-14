package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/config"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker/nsjail"
	"github.com/tianlinzz/agent-cli-gateway/workspace"
)

// Config configures the Supervisor. The supervisor owns the worker process
// lifecycle: one worker child per session, wrapped in nsjail in prod and dev,
// tracked by PID and process group, reaped on teardown.
type Config struct {
	// Mode is the runtime profile: "prod" (default), "dev", or "test". Only
	// the "test" profile may disable isolation.
	Mode string
	// Isolation is the nsjail sandbox configuration. Required must be true in
	// prod/dev (validated).
	Isolation config.IsolationConfig
	// WorkspaceRoot is the host directory workspace_ids resolve under (the
	// real controlled workspace directories are bind-mounted into the jail).
	WorkspaceRoot string
	// RuntimeDir is the host directory for per-session sockets, agent homes,
	// and generated profiles. Defaults to "gateway-run" under the cwd.
	RuntimeDir string
	// WorkerExec is the worker child executable spawned inside the jail.
	WorkerExec string
	// WorkerArgs are extra argv passed to the worker child.
	WorkerArgs []string
	// Agents contains trusted per-agent execution settings keyed by model ID.
	Agents map[string]runtime.AgentExecutionConfig
	// StartTimeout bounds the socket + handshake. Default 30s.
	StartTimeout time.Duration
	// StopGracePeriod bounds graceful shutdown (CloseSession RPC + SIGTERM)
	// before the supervisor escalates to a process-group SIGKILL. Default 10s.
	StopGracePeriod time.Duration
	// HeartbeatInterval controls runtime Worker probes. Zero disables them.
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	HeartbeatFailures int
	// SessionIdleTimeout reclaims idle Worker/Agent processes while retaining
	// Gateway/native session identity. Zero disables idle reclamation.
	SessionIdleTimeout  time.Duration
	SessionReapInterval time.Duration
}

// DefaultConfig returns the recommended supervisor config.
func DefaultConfig() Config {
	return Config{
		Mode:                config.ModeProd,
		RuntimeDir:          "gateway-run",
		StartTimeout:        30 * time.Second,
		StopGracePeriod:     10 * time.Second,
		HeartbeatInterval:   15 * time.Second,
		HeartbeatTimeout:    3 * time.Second,
		HeartbeatFailures:   3,
		SessionIdleTimeout:  2 * time.Hour,
		SessionReapInterval: time.Minute,
	}
}

func (c Config) defaults() Config {
	if c.Mode == "" {
		c.Mode = config.ModeProd
	}
	if c.RuntimeDir == "" {
		c.RuntimeDir = "gateway-run"
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 30 * time.Second
	}
	if c.StopGracePeriod <= 0 {
		c.StopGracePeriod = 10 * time.Second
	}
	if c.HeartbeatInterval > 0 {
		if c.HeartbeatTimeout <= 0 {
			c.HeartbeatTimeout = 3 * time.Second
		}
		if c.HeartbeatFailures <= 0 {
			c.HeartbeatFailures = 3
		}
	}
	if c.SessionIdleTimeout > 0 && c.SessionReapInterval <= 0 {
		c.SessionReapInterval = time.Minute
	}
	return c
}

func (c Config) validate() error {
	switch c.Mode {
	case config.ModeProd, config.ModeDev, config.ModeTest:
	default:
		return fmt.Errorf("worker: config: invalid mode %q (want %q, %q, or %q)",
			c.Mode, config.ModeProd, config.ModeDev, config.ModeTest)
	}
	if !c.Isolation.Required && c.Mode != config.ModeTest {
		return fmt.Errorf("worker: config: isolation.required=false is only allowed in mode %q; nsjail is mandatory in prod/dev", config.ModeTest)
	}
	if strings.TrimSpace(c.WorkspaceRoot) == "" {
		return fmt.Errorf("worker: config: workspace root must not be empty")
	}
	if strings.TrimSpace(c.WorkerExec) == "" {
		return fmt.Errorf("worker: config: worker executable must not be empty")
	}
	if c.Isolation.Required && strings.TrimSpace(c.Isolation.BinaryPath) == "" {
		return fmt.Errorf("worker: config: isolation.binary_path must not be empty while isolation is required")
	}
	// Layered fail-closed for seccomp (O-C2): the config layer already rejects
	// seccomp off outside the test profile, but a directly-constructed
	// Supervisor must enforce the same constraint so prod/dev can never silently
	// run without a seccomp filter. Unset policy is treated as kafel (Build
	// emits the whitelist for an empty policy).
	switch c.Isolation.Seccomp.Policy {
	case config.SeccompKafel, "":
	case config.SeccompOff:
		if c.Mode != config.ModeTest {
			return fmt.Errorf("worker: config: isolation.seccomp.policy %q is only allowed in mode %q", config.SeccompOff, config.ModeTest)
		}
	default:
		return fmt.Errorf("worker: config: isolation.seccomp.policy %q invalid (want %q or %q)", c.Isolation.Seccomp.Policy, config.SeccompKafel, config.SeccompOff)
	}
	return nil
}

// Supervisor starts, tracks, and reaps one nsjail-wrapped worker per session.
// Worker failures are isolated to the current session and can never take the
// API process down.
type Supervisor struct {
	mu       sync.Mutex
	cfg      Config
	resolver *workspace.Resolver
	spawn    spawnFunc
	sessions map[string]*workerSession
	slots    map[string]chan struct{}
	stopOnce sync.Once
	stop     chan struct{}
	reapDone chan struct{}
}

// Option customizes a Supervisor. Options are applied after construction and
// validation.
type Option func(*Supervisor)

// WithSpawner overrides the process spawner. It is a test seam: the default
// spawner is the nsjail-aware platform spawner; tests inject a stub spawner
// that forks a fake nsjail script. Fail-closed: an override cannot silently
// bypass isolation — the built-in direct spawner (spawnDirect) refuses to fork
// an unsandboxed worker when the session requires isolation, so prod/dev
// sessions are never unsandboxed through this seam.
func WithSpawner(fn spawnFunc) Option {
	return func(s *Supervisor) {
		if fn != nil {
			s.spawn = fn
		}
	}
}

// NewSupervisor builds a Supervisor. The workspace root must already exist
// (the resolver fails closed on a missing root), and the config is validated.
func NewSupervisor(cfg Config, opts ...Option) (*Supervisor, error) {
	cfg = cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	absRuntime, err := filepath.Abs(cfg.RuntimeDir)
	if err != nil {
		return nil, fmt.Errorf("worker: absolutize runtime dir: %w", err)
	}
	cfg.RuntimeDir = absRuntime

	resolver, err := workspace.NewResolver(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		cfg:      cfg,
		resolver: resolver,
		spawn:    defaultSpawner(cfg),
		sessions: make(map[string]*workerSession),
		slots:    make(map[string]chan struct{}),
		stop:     make(chan struct{}),
		reapDone: make(chan struct{}),
	}
	for name, a := range cfg.Agents {
		if a.MaxConcurrency > 0 {
			s.slots[name] = make(chan struct{}, a.MaxConcurrency)
		}
	}
	for _, o := range opts {
		o(s)
	}
	if cfg.SessionIdleTimeout > 0 {
		go s.reapIdleLoop()
	} else {
		close(s.reapDone)
	}
	return s, nil
}

// StartSession launches a session: it resolves the real workspace directory,
// creates the per-session agent home and socket directories, writes the
// nsjail profile (when isolated), forks the worker (nsjail-wrapped in
// prod/dev), performs the gRPC handshake, and starts the event bridge. It
// returns a handle implementing runtime.ExecutionHandle.
//
// Failure is fail-closed: any error (missing nsjail binary, invalid profile,
// spawn failure, handshake failure) tears the child down and never falls back
// to an unsandboxed spawn.
func (s *Supervisor) StartSession(ctx context.Context, req runtime.StartRequest) (*workerSession, error) {
	if err := validateStartRequest(req); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if _, ok := s.sessions[req.SessionID]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("worker: session %q already running", req.SessionID)
	}
	s.mu.Unlock()
	slotHeld := false
	if slot := s.slots[req.ModelID]; slot != nil {
		select {
		case slot <- struct{}{}:
			slotHeld = true
		case <-ctx.Done():
			return nil, fmt.Errorf("worker: acquire %s concurrency slot: %w", req.ModelID, ctx.Err())
		}
	}
	defer func() {
		if slotHeld {
			if slot := s.slots[req.ModelID]; slot != nil {
				<-slot
			}
		}
	}()

	// Resolve the opaque workspace_id to the real controlled directory. The
	// resolver guarantees the result stays inside the configured root, and the
	// nsjail mount namespace is the second boundary.
	wsDir, err := s.resolver.Resolve(req.CallerID, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("worker: start session %q: resolve workspace: %w", req.SessionID, err)
	}

	sessionDir := filepath.Join(s.cfg.RuntimeDir, "sessions", shortID(req.SessionID))
	socketDir := filepath.Join(s.cfg.RuntimeDir, "sockets", shortID(req.SessionID))
	agentHomeDir := filepath.Join(sessionDir, "agent-home")
	socketPath := filepath.Join(socketDir, "w.sock")
	// Unix socket paths are kernel-limited (~104 bytes on darwin, ~108 on
	// linux). A deep RuntimeDir combined with the session dir could silently
	// kill the worker at listen time, so fail closed with a clear error.
	if len(socketPath) > 100 {
		return nil, fmt.Errorf("worker: start session %q: socket path too long (%d bytes, limit 100); configure a shorter runtime dir", req.SessionID, len(socketPath))
	}
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("worker: start session %q: mkdir socket dir: %w", req.SessionID, err)
	}
	if err := os.MkdirAll(agentHomeDir, 0o700); err != nil {
		return nil, fmt.Errorf("worker: start session %q: mkdir agent home: %w", req.SessionID, err)
	}

	// Fail-closed isolation setup: the nsjail binary must exist and be
	// executable, and the profile must assemble before we fork anything.
	isolated := s.cfg.Isolation.Required
	var profilePath string
	if isolated {
		if err := nsjail.ValidateBinary(s.cfg.Isolation.BinaryPath); err != nil {
			return nil, fmt.Errorf("worker: start session %q: %w", req.SessionID, err)
		}
		prof, err := nsjail.Build(s.cfg.Isolation, nsjail.SessionLayout{
			WorkspaceDir: wsDir,
			AgentHomeDir: agentHomeDir,
			SocketDir:    socketDir,
			KeepEnv:      []string{"PATH", "HOME", "GW_WORKER_SOCKET", "GW_WORKER_SESSION_ID"},
		}, req.SessionID)
		if err != nil {
			return nil, fmt.Errorf("worker: start session %q: build nsjail profile: %w", req.SessionID, err)
		}
		profileDir := filepath.Join(sessionDir, "profile")
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			return nil, fmt.Errorf("worker: start session %q: mkdir profile dir: %w", req.SessionID, err)
		}
		profilePath = filepath.Join(profileDir, "profile.conf")
		if err := os.WriteFile(profilePath, []byte(prof.Config), 0o600); err != nil {
			return nil, fmt.Errorf("worker: start session %q: write nsjail profile: %w", req.SessionID, err)
		}
	}

	workerWorkspaceDir := wsDir
	if isolated {
		workerWorkspaceDir = s.cfg.Isolation.Mounts.WorkspaceDir
		if strings.TrimSpace(workerWorkspaceDir) == "" {
			workerWorkspaceDir = "/workspace"
		}
	}
	env := append(os.Environ(),
		"GW_WORKER_SOCKET="+socketPath,
		"GW_WORKER_SESSION_ID="+req.SessionID,
		"GW_WORKSPACE_DIR="+workerWorkspaceDir,
	)
	if isolated {
		workerAgentHomeDir := s.cfg.Isolation.Mounts.AgentHomeDir
		if strings.TrimSpace(workerAgentHomeDir) == "" {
			workerAgentHomeDir = "/agent-home"
		}
		env = append(env,
			"GW_AGENT_HOME="+workerAgentHomeDir,
			"HOME="+workerAgentHomeDir,
		)
	}
	spec := spawnSpec{
		isolated:     isolated,
		nsjailBinary: s.cfg.Isolation.BinaryPath,
		profilePath:  profilePath,
		workerExe:    s.cfg.WorkerExec,
		workerArgs:   s.cfg.WorkerArgs,
		env:          env,
		logPath:      filepath.Join(sessionDir, "worker.log"),
	}

	startCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()

	cmd, err := s.spawn(startCtx, spec)
	if err != nil {
		os.RemoveAll(sessionDir)
		os.RemoveAll(socketDir)
		return nil, fmt.Errorf("worker: start session %q: spawn: %w", req.SessionID, err)
	}

	ws := &workerSession{
		sup:          s,
		req:          req,
		events:       make(chan runtime.Event, 128),
		stopping:     make(chan struct{}),
		done:         make(chan struct{}),
		reaped:       make(chan struct{}),
		cmd:          cmd,
		outerPID:     cmd.Process.Pid,
		pgid:         cmd.Process.Pid, // Setpgid: the child is its own group leader
		sessionDir:   sessionDir,
		socketDir:    socketDir,
		socketPath:   socketPath,
		profilePath:  profilePath,
		state:        stateStarting,
		turnActive:   req.FirstInput != nil,
		lastActivity: time.Now(),
	}
	if slot := s.slots[req.ModelID]; slot != nil {
		ws.releaseSlot = func() { <-slot }
		slotHeld = false
	}
	ws.ctx, ws.cancel = context.WithCancel(context.Background())
	go ws.monitor()

	// Handshake (socket + Health + StartSession) bounded by StartTimeout.
	if err := ws.handshake(startCtx); err != nil {
		ws.terminate(fmt.Errorf("handshake: %w", err))
		select {
		case <-ws.reaped:
		case <-time.After(2 * time.Second):
			slog.Error("worker: session process not reaped after failed handshake", "session", req.SessionID, "pid", ws.outerPID)
		}
		ws.mu.Lock()
		herr := ws.err
		ws.mu.Unlock()
		return nil, herr
	}

	// Register and start the event bridge. Registration happens only after a
	// successful handshake so a crashed worker never lingers in the map.
	s.mu.Lock()
	s.sessions[req.SessionID] = ws
	s.mu.Unlock()

	// A worker that died between the handshake and registration (terminate
	// already consumed its sync.Once) must be removed, not left as a zombie.
	ws.mu.Lock()
	dead := ws.state == stateClosed
	if !dead {
		ws.state = stateRunning
	}
	ws.mu.Unlock()
	if dead {
		s.removeSession(req.SessionID)
		ws.mu.Lock()
		derr := ws.err
		ws.mu.Unlock()
		if derr != nil {
			return nil, fmt.Errorf("worker: session %q died during start: %w", req.SessionID, derr)
		}
		return nil, fmt.Errorf("worker: session %q died during start", req.SessionID)
	}

	go ws.bridge()
	if s.cfg.HeartbeatInterval > 0 {
		go ws.heartbeat()
	}

	_, workerPID := ws.snapshotClient()
	slog.Info("worker: session started",
		"session", req.SessionID,
		"model", req.ModelID,
		"owner", req.CallerID,
		"workspace_id", req.WorkspaceID,
		"pid", ws.outerPID,
		"worker_pid", workerPID,
		"isolated", isolated,
		"lifecycle", ws.lifecycle,
	)
	return ws, nil
}

// Preflight verifies the nsjail sandbox boundary before serving requests. It
// fails closed (readiness 503 in the API layer) when the nsjail binary is
// missing/non-executable or the platform cannot run the required namespaces.
func (s *Supervisor) Preflight(ctx context.Context) error {
	return nsjail.Preflight(ctx, s.cfg.Isolation)
}

// Close shuts every running session down and waits for the process groups to
// be reaped. It is used by the gateway's graceful shutdown.
func (s *Supervisor) Close(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.reapDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	sessions := make([]*workerSession, 0, len(s.sessions))
	for _, ws := range s.sessions {
		sessions = append(sessions, ws)
	}
	s.mu.Unlock()

	errCh := make(chan error, len(sessions))
	var wg sync.WaitGroup
	for _, ws := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ws.Close(ctx); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

func (s *Supervisor) reapIdleLoop() {
	defer close(s.reapDone)
	ticker := time.NewTicker(s.cfg.SessionReapInterval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			s.reapIdle(now)
		case <-s.stop:
			return
		}
	}
}

func (s *Supervisor) reapIdle(now time.Time) {
	s.mu.Lock()
	sessions := make([]*workerSession, 0, len(s.sessions))
	for _, ws := range s.sessions {
		if ws.claimIdle(now, s.cfg.SessionIdleTimeout) {
			sessions = append(sessions, ws)
		}
	}
	s.mu.Unlock()
	for _, ws := range sessions {
		slog.Info("worker: reclaiming idle session process", "session", ws.req.SessionID, "idle_timeout", s.cfg.SessionIdleTimeout)
		go func() {
			if err := ws.closeClaimed(context.Background()); err != nil {
				slog.Warn("worker: reclaim idle session process", "session", ws.req.SessionID, "error", err)
			}
		}()
	}
}

// SessionCount returns the number of currently tracked sessions.
func (s *Supervisor) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Supervisor) removeSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func validateStartRequest(req runtime.StartRequest) error {
	if strings.TrimSpace(req.SessionID) == "" {
		return fmt.Errorf("worker: start session: empty session id")
	}
	if strings.TrimSpace(req.ModelID) == "" {
		return fmt.Errorf("worker: start session %q: empty model id", req.SessionID)
	}
	if strings.Contains(req.ProviderModel, "/") {
		return fmt.Errorf("worker: start session %q: invalid provider model", req.SessionID)
	}
	if strings.TrimSpace(req.CallerID) == "" {
		return fmt.Errorf("worker: start session %q: empty owner id", req.SessionID)
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		return fmt.Errorf("worker: start session %q: empty workspace id", req.SessionID)
	}
	return nil
}

// shortID derives a short, deterministic directory token from the session id.
// Unix socket paths are limited (~104 bytes on darwin, ~108 on linux), so the
// per-session socket directory must never be built from the raw session id
// (gateway session ids can be long UUIDs). A 16-hex-char sha256 prefix keeps
// the socket path short while remaining collision-resistant in practice; the
// mapping session id -> directory lives in the workerSession record.
func shortID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------------------
// workerSession: one running worker process and its event bridge.
// ---------------------------------------------------------------------------

type sessionState int

const (
	stateStarting sessionState = iota
	stateRunning
	stateClosing
	stateClosed
)

// workerSession is the runtime.ExecutionHandle for one worker process. It
// wraps the gRPC client, the process-group lifecycle, and the canonical event
// bridge. All state is guarded by mu; termination is idempotent via once.
type workerSession struct {
	once        sync.Once
	releaseSlot func()

	sup    *Supervisor
	req    runtime.StartRequest
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	state        sessionState
	closing      bool
	lifecycle    string
	err          error
	turnActive   bool
	idleReaping  bool
	lastActivity time.Time

	client   *client
	events   chan runtime.Event
	stopping chan struct{} // internal signal: teardown started, stop event bridge
	done     chan struct{} // external signal: deregistration and cleanup completed
	reaped   chan struct{} // closed when the direct child (cmd) is reaped

	cmd       *exec.Cmd
	outerPID  int // immutable after construction: nsjail PID when isolated, worker PID when direct
	workerPID int // guarded by mu: worker PID reported at handshake
	pgid      int // immutable after construction: process group of the spawned child (= outerPID with Setpgid)

	sessionDir  string
	socketDir   string
	socketPath  string
	profilePath string
}

// Events returns the canonical event stream. It is closed when the session
// terminates, which is the signal that the worker is gone.
func (ws *workerSession) Events() <-chan runtime.Event {
	return ws.events
}

// snapshotClient returns the current gRPC client and worker PID under the lock.
// The monitor goroutine starts before handshake assigns client/workerPID, so
// any read that can race with handshake must go through here. Callers must not
// hold the lock when acting on the snapshot (RPC/Close/Wait).
func (ws *workerSession) snapshotClient() (*client, int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.client, ws.workerPID
}

// clearClient closes c and nils the cached client under the lock. Used by
// handshake failure paths so the monitor/terminate never see a stale client.
func (ws *workerSession) clearClient(c *client) {
	if c != nil {
		c.Close()
	}
	ws.mu.Lock()
	ws.client = nil
	ws.mu.Unlock()
}

// Send delivers a turn to the worker.
func (ws *workerSession) Send(ctx context.Context, input runtime.Input) error {
	ws.mu.Lock()
	state, err := ws.state, ws.err
	switch state {
	case stateClosed:
		ws.mu.Unlock()
		if err != nil {
			return fmt.Errorf("worker: session %q: %w", ws.req.SessionID, err)
		}
		return fmt.Errorf("worker: session %q is closed", ws.req.SessionID)
	case stateRunning:
		ws.turnActive = true
		ws.lastActivity = time.Now()
		ws.mu.Unlock()
	default:
		ws.mu.Unlock()
		return fmt.Errorf("worker: session %q is not running (state %d)", ws.req.SessionID, state)
	}
	client, _ := ws.snapshotClient()
	if client == nil {
		ws.mu.Lock()
		ws.turnActive = false
		ws.lastActivity = time.Now()
		ws.mu.Unlock()
		return fmt.Errorf("worker: session %q has no client (not started)", ws.req.SessionID)
	}
	if err := client.SendInput(ctx, ws.req.SessionID, input); err != nil {
		ws.mu.Lock()
		ws.turnActive = false
		ws.lastActivity = time.Now()
		ws.mu.Unlock()
		return err
	}
	return nil
}

func (ws *workerSession) claimIdle(now time.Time, timeout time.Duration) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.state != stateRunning || ws.turnActive || ws.idleReaping || now.Sub(ws.lastActivity) < timeout {
		return false
	}
	ws.idleReaping = true
	ws.state = stateClosing
	ws.closing = true
	return true
}

// Abort delegates turn cancellation to the native session. Successful native
// cancellation keeps the Worker alive; adapters may terminate themselves only
// when their protocol cannot cancel or cancellation escalation fails.
func (ws *workerSession) Abort(ctx context.Context) error {
	ws.mu.Lock()
	state, err := ws.state, ws.err
	ws.mu.Unlock()
	if state != stateRunning {
		if err != nil {
			return err
		}
		return nil
	}
	client, _ := ws.snapshotClient()
	if client == nil {
		return nil
	}
	return client.Abort(ctx, ws.req.SessionID)
}

// Close tears the session down: CloseSession RPC, SIGTERM to the process
// group, bounded wait, then SIGKILL escalation, then reap and cleanup.
func (ws *workerSession) Close(ctx context.Context) error {
	ws.mu.Lock()
	if ws.state == stateClosed {
		err := ws.err
		ws.mu.Unlock()
		return err
	}
	if ws.state == stateClosing {
		done := ws.done
		ws.mu.Unlock()
		select {
		case <-done:
			return ws.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ws.state = stateClosing
	ws.closing = true
	ws.mu.Unlock()
	return ws.closeClaimed(ctx)
}

// closeClaimed performs teardown after the caller atomically moved the
// session to stateClosing. Idle reclamation uses it so no new Send can enter
// between selecting a session and starting asynchronous teardown.
func (ws *workerSession) closeClaimed(ctx context.Context) error {
	var closeErr error
	if client, _ := ws.snapshotClient(); client != nil {
		cctx, cancel := context.WithTimeout(ctx, ws.sup.cfg.StopGracePeriod)
		closeErr = client.CloseSession(cctx, ws.req.SessionID)
		cancel()
	}

	// Graceful: SIGTERM to the whole group (nsjail + worker + agent CLI).
	ws.killGroup(syscall.SIGTERM)

	select {
	case <-ws.reaped:
	case <-time.After(ws.sup.cfg.StopGracePeriod):
		slog.Warn("worker: process group did not exit after SIGTERM, sending SIGKILL",
			"session", ws.req.SessionID, "pgid", ws.pgid)
		ws.killGroup(syscall.SIGKILL)
		select {
		case <-ws.reaped:
		case <-time.After(2 * time.Second):
			slog.Error("worker: process group did not reap after SIGKILL",
				"session", ws.req.SessionID, "pgid", ws.pgid)
			return fmt.Errorf("worker: session %q process group %d did not reap after SIGKILL", ws.req.SessionID, ws.pgid)
		}
	}

	ws.terminate(closeErr)

	ws.mu.Lock()
	err := ws.err
	ws.mu.Unlock()
	if err == nil {
		err = closeErr
	}
	return err
}

// Err returns the terminal error of the session (nil if it ended cleanly).
func (ws *workerSession) Err() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.err
}

// Closed reports whether the session has terminated (the worker exited or was
// torn down). It backs the API layer's dead-handle recovery: Send on a closed
// handle fails terminally, and the API drops the handle and starts a fresh
// execution instead of wedging the session with 500s.
func (ws *workerSession) Closed() bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.state == stateClosed
}

// Done closes after the worker execution reaches its terminal state. It lets
// the API distinguish a transport failure racing with process reaping from a
// transient Send error without importing worker implementation details.
func (ws *workerSession) Done() <-chan struct{} { return ws.done }

// Terminating reports that teardown has started. Once true, callers may wait
// on Done for the stronger guarantee that deregistration and cleanup finished.
func (ws *workerSession) Terminating() bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.state == stateClosing || ws.state == stateClosed
}

// monitor waits for the direct child (nsjail wrapper, or the worker itself in
// direct mode) to exit, reaps any orphaned group members, and terminates the
// session. This is the path that catches worker crashes and nsjail-wrapper
// deaths: a failure is isolated to this session.
func (ws *workerSession) monitor() {
	err := ws.cmd.Wait()

	ws.mu.Lock()
	closing := ws.closing
	ws.mu.Unlock()
	if err != nil && !closing {
		ws.mu.Lock()
		ws.err = fmt.Errorf("worker: process %d exited unexpectedly: %w", ws.outerPID, err)
		ws.mu.Unlock()
		slog.Warn("worker: session process exited", "session", ws.req.SessionID, "pid", ws.outerPID, "error", err)
	}

	// If the nsjail wrapper died first, the worker is orphaned in the group:
	// kill the group so nothing survives the session.
	ws.killGroup(syscall.SIGKILL)
	close(ws.reaped)
	ws.terminate(nil)
}

// heartbeat detects a wedged or disconnected Worker after the initial
// handshake. Consecutive successes reset the failure count; only the
// configured threshold publishes terminal state and triggers lazy recovery
// on the next Gateway turn.
func (ws *workerSession) heartbeat() {
	ticker := time.NewTicker(ws.sup.cfg.HeartbeatInterval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), ws.sup.cfg.HeartbeatTimeout)
			cl, expectedPID := ws.snapshotClient()
			if cl == nil {
				cancel()
				continue
			}
			_, pid, err := cl.Health(ctx)
			cancel()
			if err == nil && int(pid) != expectedPID {
				err = fmt.Errorf("worker pid changed from %d to %d", expectedPID, pid)
			}
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures < ws.sup.cfg.HeartbeatFailures {
				slog.Warn("worker: heartbeat failed", "session", ws.req.SessionID, "failures", failures, "threshold", ws.sup.cfg.HeartbeatFailures, "error", err)
				continue
			}
			ws.terminate(fmt.Errorf("worker heartbeat failed %d consecutive times: %w", failures, err))
			return
		case <-ws.stopping:
			return
		}
	}
}

// bridge forwards canonical events from the worker's gRPC stream into the
// handle's Events channel. It is the only writer of ws.events and closes it
// when the session ends. terminations cancel ws.ctx so a blocked Recv
// unblocks and the bridge can exit.
func (ws *workerSession) bridge() {
	defer close(ws.events)

	client, _ := ws.snapshotClient()
	if client == nil {
		ws.terminate(fmt.Errorf("open event stream: no client"))
		return
	}
	stream, err := client.StreamEvents(ws.ctx, ws.req.SessionID)
	if err != nil {
		ws.terminate(fmt.Errorf("open event stream: %w", err))
		return
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				// The worker ended the session cleanly.
				ws.terminate(nil)
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// Termination initiated by us; nothing more to report.
				ws.terminate(nil)
			default:
				// Transport failure: the worker likely crashed. But if we
				// are already tearing this session down (Close in progress),
				// the process was killed by us and the abrupt stream end is
				// the expected result — not a session error. Without this
				// guard a Close that escalates to SIGKILL (worker ignoring
				// SIGTERM) would surface "event stream ended" as the
				// terminal error of a successfully-torn-down session.
				ws.mu.Lock()
				closing := ws.closing
				ws.mu.Unlock()
				if closing {
					ws.terminate(nil)
					return
				}
				ws.terminate(fmt.Errorf("event stream ended: %w", err))
			}
			return
		}
		ev := fromFrame(frame)
		if ev.Type == runtime.EventFinish || ev.Type == runtime.EventError {
			ws.mu.Lock()
			ws.turnActive = false
			ws.lastActivity = time.Now()
			ws.mu.Unlock()
		}
		select {
		case ws.events <- ev:
		case <-ws.stopping:
			return
		}
	}
}

// terminate is the idempotent teardown path. The internal stopping signal is
// published first so the event bridge exits; the external Done/Closed state is
// published only after deregistration and runtime-path cleanup, making an
// immediate same-session restart safe.
func (ws *workerSession) terminate(err error) {
	ws.once.Do(func() {
		ws.cancel()
		ws.mu.Lock()
		if err != nil && ws.err == nil {
			ws.err = err
		}
		ws.state = stateClosing
		ws.mu.Unlock()

		ws.killGroup(syscall.SIGKILL)
		if ws.stopping != nil {
			close(ws.stopping)
		}
		// Wait for the process tree to be reaped before removing runtime
		// directories. A still-dying worker may hold the unix socket or
		// worker.log, racing RemoveAll and leaving stale files that cause
		// EADDRINUSE on the next StartSession (O-B5). monitor() and
		// closeClaimed already close reaped before reaching here, so this
		// wait is instant on those paths; heartbeat/bridge benefit from it.
		if ws.reaped != nil {
			select {
			case <-ws.reaped:
			case <-time.After(2 * time.Second):
				slog.Warn("worker: process not reaped before cleanup", "session", ws.req.SessionID, "pid", ws.outerPID)
			}
		}
		// Snapshot client under the lock: the monitor goroutine (started before
		// handshake) can reach here while handshake is still assigning it.
		ws.mu.Lock()
		client := ws.client
		ws.mu.Unlock()
		if client != nil {
			client.Close()
		}
		ws.sup.removeSession(ws.req.SessionID)
		if ws.releaseSlot != nil {
			ws.releaseSlot()
		}
		if ws.sessionDir != "" {
			if err := os.RemoveAll(ws.sessionDir); err != nil {
				slog.Warn("worker: cleanup session dir", "session", ws.req.SessionID, "error", err)
			}
		}
		// The per-session socket dir holds w.sock, the worker's listen path.
		// net.Listen("unix", ...) fails with EADDRINUSE on a stale socket file,
		// so it MUST be removed on every terminal path or a later StartSession
		// with the same SessionID dies at bind time.
		if ws.socketDir != "" {
			if err := os.RemoveAll(ws.socketDir); err != nil {
				slog.Warn("worker: cleanup socket dir", "session", ws.req.SessionID, "error", err)
			}
		}
		ws.mu.Lock()
		ws.state = stateClosed
		ws.mu.Unlock()
		close(ws.done)
	})
}

// killGroup is defined in the platform files:
//   - spawn_unix.go (!windows): POSIX process-group signals (nsjail + worker +
//     agent CLI) via syscall.Kill.
//   - spawn_windows.go (windows): taskkill /T /F process-tree kill (no POSIX
//     process groups; nsjail is Linux-only so Windows is dev-only).
//
// With per-jail PID namespaces (config.CloneNewPID, default on), killing the
// nsjail wrapper (PID 1 inside the namespace) makes the kernel SIGKILL every
// process in that namespace, so an Agent CLI that escaped into its own process
// group still dies with the wrapper. killGroup remains defense-in-depth for the
// shared-PID-namespace fallback and direct (test) mode, where the CLI's
// separate process group is invisible to the wrapper's group kill.

// handshake waits for the worker socket to appear, dials, performs the
// Health handshake, and starts the session. It is bounded by ctx.
func (ws *workerSession) handshake(ctx context.Context) error {
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			if _, err := os.Stat(ws.socketPath); err == nil {
				goto dial
			}
		case <-ws.reaped:
			return fmt.Errorf("session process %d exited before serving", ws.outerPID)
		case <-ctx.Done():
			return fmt.Errorf("socket %s never appeared: %w", ws.socketPath, ctx.Err())
		}
	}

dial:
	c, err := dial(ctx, UnixSocket(ws.socketPath))
	if err != nil {
		return fmt.Errorf("dial worker: %w", err)
	}
	// client/workerPID are read by the monitor goroutine (started before
	// handshake) if the process exits mid-handshake, so their writes are
	// guarded by ws.mu to avoid a data race with terminate.
	ws.mu.Lock()
	ws.client = c
	ws.mu.Unlock()

	retry := time.NewTicker(50 * time.Millisecond)
	defer retry.Stop()
	var lastErr error
	for {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		version, pid, herr := c.Health(hctx)
		cancel()
		if herr == nil {
			ws.mu.Lock()
			ws.workerPID = int(pid)
			ws.mu.Unlock()
			slog.Debug("worker: handshake ok", "session", ws.req.SessionID, "version", version, "worker_pid", pid)
			break
		}
		lastErr = herr
		select {
		case <-ws.reaped:
			ws.clearClient(c)
			return fmt.Errorf("session process %d exited during handshake", ws.outerPID)
		case <-ctx.Done():
			ws.clearClient(c)
			return fmt.Errorf("health handshake failed: %v (last error: %v)", ctx.Err(), lastErr)
		case <-retry.C:
		}
	}

	startReq := toStartSessionReq(ws.req)
	if cfg, ok := ws.sup.cfg.Agents[ws.req.ModelID]; ok {
		startReq.AgentConfig = cfg
	}
	mode, err := c.StartSession(ctx, startReq)
	if err != nil {
		ws.clearClient(c)
		return fmt.Errorf("start session rpc: %w", err)
	}
	ws.mu.Lock()
	ws.lifecycle = mode
	ws.mu.Unlock()
	return nil
}

func toStartSessionReq(req runtime.StartRequest) StartSessionReq {
	r := StartSessionReq{
		ModelID:       req.ModelID,
		SessionID:     req.SessionID,
		CallerID:      req.CallerID,
		WorkspaceID:   req.WorkspaceID,
		ProviderModel: req.ProviderModel,
		Metadata:      req.Metadata,
	}
	if req.FirstInput != nil {
		in := *req.FirstInput
		r.FirstInput = &in
	}
	return r
}
