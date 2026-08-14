package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/config"
	grt "github.com/tianlinzz/agent-cli-gateway/runtime"
)

// TestMain builds the stub worker executable once for the whole package. The
// stub worker is a real Go binary (worker/testworker) that serves the Worker
// gRPC service on the per-session Unix socket, so the tests exercise the real
// RPC contract, real process lifecycle, and real signals end to end.
func TestMain(m *testing.M) {
	var err error
	stubWorkerBin, stubWorkerDir, err = buildStubWorker()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(stubWorkerDir)
	os.Exit(code)
}

var (
	stubWorkerBin string
	stubWorkerDir string
)

func buildStubWorker() (bin, dir string, err error) {
	// Locate the module root: this file lives in <root>/worker/.
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(file))
	dir, err = os.MkdirTemp("", "gateway-stub-worker-*")
	if err != nil {
		return "", "", err
	}
	bin = filepath.Join(dir, "testworker")
	cmd := exec.Command("go", "build", "-o", bin, "./worker/testworker")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("build stub worker: %w\n%s", err, out)
	}
	return bin, dir, nil
}

// testRuntimeDir returns a short host directory for the supervisor runtime
// state. The per-session Unix socket path has a ~104-byte kernel limit, and
// Go's t.TempDir() is far too deep, so the runtime dir must be a short path.
func testRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("gwrt-%d", os.Getpid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newTestSupervisor builds a supervisor rooted in a temp dir. The default
// config is test mode with isolation disabled; mutCfg can override fields
// (e.g. switch to dev mode + isolation for the nsjail-wrapper tests).
func newTestSupervisor(t *testing.T, mutCfg func(*Config), opts ...Option) *Supervisor {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Mode = config.ModeTest
	cfg.Isolation = config.IsolationConfig{
		Required: false,
		// Per-jail PID namespace is a security boundary; keep it on by default
		// so dev/prod overrides satisfy the fail-closed validation (F6).
		CloneNewPID: true,
		Mounts: config.MountsConfig{
			WorkspaceDir: "/workspace",
			AgentHomeDir: "/agent-home",
			TmpDir:       "/tmp",
		},
	}
	cfg.WorkspaceRoot = t.TempDir()
	cfg.RuntimeDir = testRuntimeDir(t)
	cfg.WorkerExec = stubWorkerBin
	cfg.StartTimeout = 5 * time.Second
	cfg.StopGracePeriod = 2 * time.Second
	if mutCfg != nil {
		mutCfg(&cfg)
	}
	sup, err := NewSupervisor(cfg, opts...)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sup.Close(ctx)
	})
	return sup
}

func testRequest(sessionID string) grt.StartRequest {
	return grt.StartRequest{
		ModelID:     "test-model",
		SessionID:   sessionID,
		CallerID:    "owner-1",
		WorkspaceID: "ws-1",
	}
}

// waitEvent reads events until one of the given type arrives and returns it.
func waitEvent(t *testing.T, ws *workerSession, typ grt.EventType, timeout time.Duration) grt.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ws.events:
			if !ok {
				t.Fatalf("events channel closed while waiting for %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %s", typ)
		}
	}
}

// waitEventsClosed waits until the session's events channel closes.
func waitEventsClosed(t *testing.T, ws *workerSession, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-ws.events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("events channel did not close within %s", timeout)
		}
	}
}

// pidAlive is defined in proc_test_unix.go / proc_test_windows.go.

func writeFakeNsjail(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nsjail")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake nsjail: %v", err)
	}
	return p
}

// fakeNsjailExec replaces itself with the worker: argv is
// `nsjail -Mo --config <profile> -- <worker> <args...>`.
const fakeNsjailExec = `#!/bin/sh
# fake nsjail (exec): drop the nsjail-specific prefix and exec the worker.
shift 4
exec "$@"
`

// fakeNsjailWrapper keeps the worker as its child and stays alive, so the
// supervisor can observe the nsjail process dying independently of the worker
// (the "wrapper crash reaps the whole group" scenario).
const fakeNsjailWrapper = `#!/bin/sh
# fake nsjail (wrapper): run the worker as a child and stay alive.
shift 4
"$@" &
child=$!
trap 'kill "$child" 2>/dev/null; exit 0' TERM INT
wait "$child"
exit $?
`

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// TestSupervisor_ConcurrentLifecycleRaceConditioning exercises handshake, Send,
// Abort, Close, heartbeat, and terminate/killGroup interleaving concurrently.
// It is a race-conditioning guard for the client/workerPID publication contract
// (O-B3): all access goes through snapshotClient/clearClient under ws.mu, so
// this must stay clean under -race -count=N.
func TestSupervisor_ConcurrentLifecycleRaceConditioning(t *testing.T) {
	sup := newTestSupervisor(t, func(c *Config) {
		c.HeartbeatInterval = 20 * time.Millisecond
		c.HeartbeatTimeout = 10 * time.Millisecond
		c.HeartbeatFailures = 5
	})

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ws, err := sup.StartSession(ctx, testRequest("race-"+strconv.Itoa(i)))
			if err != nil {
				return // a concurrent teardown may win; that is acceptable here
			}
			// Drain the event stream so the bridge never blocks on a full channel.
			drainDone := make(chan struct{})
			go func() {
				defer close(drainDone)
				for range ws.Events() {
				}
			}()
			// Interleave Send and Abort with the monitor/heartbeat goroutines.
			var ops sync.WaitGroup
			ops.Add(2)
			go func() {
				defer ops.Done()
				_ = ws.Send(ctx, grt.Input{Messages: []grt.Message{{Role: "user", Content: "x"}}})
			}()
			go func() { defer ops.Done(); _ = ws.Abort(ctx) }()
			_ = ws.Close(ctx)
			ops.Wait()
			<-drainDone
		}()
	}
	wg.Wait()
}

func TestSupervisor_StartCloseDirect(t *testing.T) {
	sup := newTestSupervisor(t, nil)
	ws, err := sup.StartSession(context.Background(), testRequest("s1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	waitEvent(t, ws, grt.EventStatus, 5*time.Second) // "started"

	if err := ws.Send(context.Background(), grt.Input{
		Messages: []grt.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(t, ws, grt.EventText, 5*time.Second); ev.Text != "echo:hello" {
		t.Errorf("text event = %q, want %q", ev.Text, "echo:hello")
	}
	waitEvent(t, ws, grt.EventFinish, 5*time.Second)

	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pidAlive(ws.outerPID) {
		t.Errorf("worker pid %d still alive after Close", ws.outerPID)
	}
	waitEventsClosed(t, ws, 5*time.Second)
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("sessions left in supervisor after close: %d", n)
	}
}

func TestSupervisor_IdleReaperClosesOnlyIdleWorker(t *testing.T) {
	sup := newTestSupervisor(t, func(c *Config) {
		c.SessionIdleTimeout = 120 * time.Millisecond
		c.SessionReapInterval = 20 * time.Millisecond
		c.HeartbeatInterval = 0
	})
	ws, err := sup.StartSession(context.Background(), testRequest("idle-reap-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, time.Second)
	waitEventsClosed(t, ws, 2*time.Second)
	if n := sup.SessionCount(); n != 0 {
		t.Fatalf("idle worker still registered: %d", n)
	}
}

func TestWorkerSession_IdleClaimPublishesTerminationBeforeAsyncClose(t *testing.T) {
	sup := newTestSupervisor(t, func(c *Config) {
		c.SessionIdleTimeout = time.Hour
		c.HeartbeatInterval = 0
	})
	ws, err := sup.StartSession(context.Background(), testRequest("idle-claim-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, time.Second)
	if !ws.claimIdle(time.Now().Add(2*time.Hour), time.Hour) {
		t.Fatal("idle worker was not claimed")
	}
	if !ws.Terminating() {
		t.Fatal("claimed idle worker must publish terminating before asynchronous close")
	}
	if err := ws.Send(context.Background(), grt.Input{Messages: []grt.Message{{Role: "user", Content: "race"}}}); err == nil {
		t.Fatal("claimed idle worker accepted a new turn")
	}
	go ws.closeClaimed(context.Background())
	select {
	case <-ws.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("claimed idle worker did not finish closing")
	}
}

func TestSupervisor_IdleReaperExemptsActiveTurn(t *testing.T) {
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "slow-echo")
	sup := newTestSupervisor(t, func(c *Config) {
		c.SessionIdleTimeout = 80 * time.Millisecond
		c.SessionReapInterval = 10 * time.Millisecond
		c.HeartbeatInterval = 0
	})
	ws, err := sup.StartSession(context.Background(), testRequest("idle-active-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, time.Second)
	if err := ws.Send(context.Background(), grt.Input{Messages: []grt.Message{{Role: "user", Content: "long"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	select {
	case <-ws.Done():
		t.Fatal("idle reaper closed worker with an active turn")
	default:
	}
}

func TestSupervisor_CompletedTurnStaysAliveBeforeIdleTimeout(t *testing.T) {
	sup := newTestSupervisor(t, func(c *Config) {
		c.SessionIdleTimeout = 500 * time.Millisecond
		c.SessionReapInterval = 20 * time.Millisecond
		c.HeartbeatInterval = 0
	})
	ws, err := sup.StartSession(context.Background(), testRequest("idle-finish-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, time.Second)
	if err := ws.Send(context.Background(), grt.Input{Messages: []grt.Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(t, ws, grt.EventFinish, time.Second)
	time.Sleep(100 * time.Millisecond)
	select {
	case <-ws.Done():
		t.Fatal("completed worker was reclaimed before idle timeout")
	default:
	}
}

func TestSupervisor_HeartbeatTerminatesAtConsecutiveFailureThreshold(t *testing.T) {
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "fail-health-after-start")
	logPath := filepath.Join(t.TempDir(), "worker.log")
	t.Setenv("GW_TESTWORKER_LOG", logPath)
	sup := newTestSupervisor(t, func(c *Config) {
		c.SessionIdleTimeout = 0
		c.HeartbeatInterval = 40 * time.Millisecond
		c.HeartbeatTimeout = 20 * time.Millisecond
		c.HeartbeatFailures = 3
	})
	ws, err := sup.StartSession(context.Background(), testRequest("heartbeat-fail-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, time.Second)

	waitForLogCount(t, logPath, "health", 3, time.Second) // handshake + 2 failures
	select {
	case <-ws.Done():
		t.Fatal("worker terminated before consecutive heartbeat failure threshold")
	default:
	}

	select {
	case <-ws.Done():
	case <-time.After(time.Second):
		t.Fatal("worker did not terminate after heartbeat failure threshold")
	}
}

func waitForLogCount(t *testing.T, path, line string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		count := 0
		for _, got := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if got == line {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log %s did not contain %q %d times", path, line, want)
}

func TestSupervisor_DevProfileUsesNsjailWrapper(t *testing.T) {
	fakeNsjail := writeFakeNsjail(t, fakeNsjailExec)
	var (
		mu    sync.Mutex
		specs []spawnSpec
	)
	sup := newTestSupervisor(t, func(c *Config) {
		c.Mode = config.ModeDev
		c.Isolation.Required = true
		c.Isolation.BinaryPath = fakeNsjail
		c.Isolation.Mounts = config.MountsConfig{
			WorkspaceDir: "/workspace",
			AgentHomeDir: "/agent-home",
			TmpDir:       "/tmp",
		}
	}, WithSpawner(func(ctx context.Context, spec spawnSpec) (*exec.Cmd, error) {
		mu.Lock()
		specs = append(specs, spec)
		mu.Unlock()
		return startCommand(ctx, buildNsjailCommand(spec.nsjailBinary, spec.profilePath, spec.workerExe, spec.workerArgs), spec.env, spec.logPath)
	}))

	ws, err := sup.StartSession(context.Background(), testRequest("dev-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)
	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(specs) != 1 {
		t.Fatalf("spawner called %d times, want 1", len(specs))
	}
	sp := specs[0]
	if !sp.isolated {
		t.Error("dev profile must spawn the worker wrapped in nsjail (isolated=true)")
	}
	if sp.profilePath == "" {
		t.Error("dev profile must assemble and write an nsjail profile")
	}
	if sp.nsjailBinary != fakeNsjail {
		t.Errorf("nsjail binary = %q, want %q", sp.nsjailBinary, fakeNsjail)
	}
	if sp.workerExe != stubWorkerBin {
		t.Errorf("worker exe = %q, want %q", sp.workerExe, stubWorkerBin)
	}
}

func TestSupervisor_DirectSpawnMapsWorkspaceAndPreservesHostHome(t *testing.T) {
	var got spawnSpec
	sup := newTestSupervisor(t, nil, WithSpawner(func(ctx context.Context, spec spawnSpec) (*exec.Cmd, error) {
		got = spec
		return spawnDirect(ctx, spec)
	}))

	ws, err := sup.StartSession(context.Background(), testRequest("direct-paths-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wantWorkspace, err := sup.resolver.Resolve("owner-1", "ws-1")
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	assertEnvValue(t, got.env, "GW_WORKSPACE_DIR", wantWorkspace)
	assertEnvValue(t, got.env, "HOME", os.Getenv("HOME"))
	assertEnvMissing(t, got.env, "GW_AGENT_HOME")
}

func assertEnvValue(t *testing.T, env []string, key, want string) {
	t.Helper()
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			if got := strings.TrimPrefix(env[i], prefix); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("%s is missing from worker environment", key)
}

func assertEnvMissing(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			t.Fatalf("%s must not be overridden for a direct worker", key)
		}
	}
}

func TestSupervisor_NsjailCommandLine(t *testing.T) {
	got := buildNsjailCommand("/usr/bin/nsjail", "/profiles/s1.conf", "/bin/worker", []string{"--flag"})
	want := []string{"/usr/bin/nsjail", "-Mo", "--config", "/profiles/s1.conf", "--", "/bin/worker", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nsjail argv = %v, want %v", got, want)
	}
	gotDirect := buildDirectCommand("/bin/worker", []string{"a"})
	if want := []string{"/bin/worker", "a"}; !reflect.DeepEqual(gotDirect, want) {
		t.Errorf("direct argv = %v, want %v", gotDirect, want)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed isolation
// ---------------------------------------------------------------------------

func TestSupervisor_FailClosedNsjailBinaryMissing(t *testing.T) {
	sup := newTestSupervisor(t, func(c *Config) {
		c.Mode = config.ModeDev
		c.Isolation.Required = true
		c.Isolation.BinaryPath = filepath.Join(t.TempDir(), "does-not-exist")
	})
	_, err := sup.StartSession(context.Background(), testRequest("s-missing"))
	if err == nil {
		t.Fatal("StartSession must fail closed when the nsjail binary is missing")
	}
	if !strings.Contains(err.Error(), "nsjail") {
		t.Errorf("error should mention nsjail: %v", err)
	}
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("failed session must not be registered, got %d", n)
	}
}

func TestSupervisor_FailClosedNsjailBinaryNotExecutable(t *testing.T) {
	nonExe := filepath.Join(t.TempDir(), "nsjail")
	if err := os.WriteFile(nonExe, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := newTestSupervisor(t, func(c *Config) {
		c.Mode = config.ModeDev
		c.Isolation.Required = true
		c.Isolation.BinaryPath = nonExe
	})
	if _, err := sup.StartSession(context.Background(), testRequest("s-noexec")); err == nil {
		t.Fatal("StartSession must fail closed when the nsjail binary is not executable")
	}
}

func TestSupervisor_JailLaunchFailureFailsClosed(t *testing.T) {
	badNsjail := writeFakeNsjail(t, "#!/bin/sh\nexit 1\n")
	sup := newTestSupervisor(t, func(c *Config) {
		c.Mode = config.ModeDev
		c.Isolation.Required = true
		c.Isolation.BinaryPath = badNsjail
	}, WithSpawner(func(ctx context.Context, spec spawnSpec) (*exec.Cmd, error) {
		return startCommand(ctx, buildNsjailCommand(spec.nsjailBinary, spec.profilePath, spec.workerExe, spec.workerArgs), spec.env, spec.logPath)
	}))
	_, err := sup.StartSession(context.Background(), testRequest("bad-jail"))
	if err == nil {
		t.Fatal("StartSession must fail when the jail dies at launch")
	}
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("failed session must not be registered, got %d", n)
	}
}

// TestSupervisor_DirectSpawnRefusedWhenIsolationRequired guards the
// fail-closed isolation hole: WithSpawner(spawnDirect) + Isolation.Required=true
// + non-test mode used to fork the worker UNSANDBOXED because spawnDirect never
// checked spec.isolated. spawnDirect must refuse so NO reachable code path
// forks an unsandboxed worker when isolation is required outside test mode.
func TestSupervisor_DirectSpawnRefusedWhenIsolationRequired(t *testing.T) {
	fakeNsjail := writeFakeNsjail(t, fakeNsjailExec)
	sup := newTestSupervisor(t, func(c *Config) {
		c.Mode = config.ModeDev
		c.Isolation.Required = true
		c.Isolation.BinaryPath = fakeNsjail
	}, WithSpawner(spawnDirect))

	_, err := sup.StartSession(context.Background(), testRequest("unsandboxed-1"))
	if err == nil {
		t.Fatal("StartSession must refuse a direct (unsandboxed) spawner when isolation is required in dev/prod")
	}
	if !strings.Contains(err.Error(), "unsandboxed") {
		t.Errorf("refusal error should state the unsandboxed guard: %v", err)
	}
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("refused session must not be registered, got %d", n)
	}
}

// TestSupervisor_SeccompOffRejectedOutsideTestProfile is a regression test for
// the layered fail-closed gap (O-C2): the config layer rejects seccomp off
// outside the test profile, but a directly-constructed Supervisor previously
// accepted prod/dev + seccomp off and would run without a seccomp filter. The
// supervisor's own config validation must enforce the same constraint.
func TestSupervisor_SeccompOffRejectedOutsideTestProfile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = config.ModeProd
	cfg.WorkspaceRoot = t.TempDir()
	cfg.RuntimeDir = testRuntimeDir(t)
	cfg.WorkerExec = stubWorkerBin
	cfg.Isolation = config.IsolationConfig{
		Required:   true,
		BinaryPath: "/usr/local/bin/nsjail",
		Seccomp:    config.SeccompConfig{Policy: config.SeccompOff},
	}
	if _, err := NewSupervisor(cfg); err == nil {
		t.Fatal("NewSupervisor must reject seccomp off in prod mode")
	} else if !strings.Contains(err.Error(), "seccomp") {
		t.Errorf("seccomp-off rejection should mention seccomp, got %v", err)
	}

	// The same config in test mode is accepted (the only profile allowed to
	// disable the filter).
	cfg.Mode = config.ModeTest
	cfg.Isolation.Required = false
	if _, err := NewSupervisor(cfg); err != nil {
		t.Errorf("test profile may disable seccomp, got %v", err)
	}
}

// TestSupervisor_CloneNewPIDRequiredOutsideTestProfile is a regression test for
// the PID-namespace config hole (code-review F6): a directly-constructed
// Supervisor must not silently disable the per-jail PID namespace in prod/dev.
func TestSupervisor_CloneNewPIDRequiredOutsideTestProfile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = config.ModeProd
	cfg.WorkspaceRoot = t.TempDir()
	cfg.RuntimeDir = testRuntimeDir(t)
	cfg.WorkerExec = stubWorkerBin
	cfg.Isolation = config.IsolationConfig{
		Required:   true,
		BinaryPath: "/usr/local/bin/nsjail",
		Seccomp:    config.SeccompConfig{Policy: config.SeccompKafel},
	}
	if _, err := NewSupervisor(cfg); err == nil {
		t.Fatal("NewSupervisor must reject clone_newpid=false in prod mode")
	} else if !strings.Contains(err.Error(), "clone_newpid") {
		t.Errorf("clone_newpid rejection should mention clone_newpid, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Handshake failure and reaping
// ---------------------------------------------------------------------------

func TestSupervisor_HandshakeTimeoutReapsChild(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "hang")
	t.Setenv("GW_TESTWORKER_PIDFILE", pidfile)

	sup := newTestSupervisor(t, func(c *Config) { c.StartTimeout = 1 * time.Second })
	start := time.Now()
	_, err := sup.StartSession(context.Background(), testRequest("hang-1"))
	if err == nil {
		t.Fatal("StartSession must fail when the worker never serves")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("handshake failure took too long: %v", elapsed)
	}

	data, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("read pidfile: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 {
		t.Fatalf("bad pid from pidfile %q", data)
	}
	if pidAlive(pid) {
		t.Errorf("hung worker pid %d is still alive after the failed handshake", pid)
	}
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("failed session must not be registered, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Worker crash isolation (worker failure must not take the API process down)
// ---------------------------------------------------------------------------

func TestSupervisor_WorkerCrashDoesNotKillSupervisor(t *testing.T) {
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "crash-after-start")
	sup := newTestSupervisor(t, nil)

	ws, err := sup.StartSession(context.Background(), testRequest("crash-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second) // "started"
	// The worker now exits on its own (~300ms). The event stream must close
	// and the session must be removed from the supervisor.
	waitEventsClosed(t, ws, 10*time.Second)

	ws.mu.Lock()
	crashed := ws.err
	ws.mu.Unlock()
	if crashed == nil {
		t.Error("session must record the worker crash as its terminal error")
	}
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("crashed session must be removed, got %d", n)
	}

	// The supervisor must still be fully functional: start a second session.
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "")
	ws2, err := sup.StartSession(context.Background(), testRequest("crash-2"))
	if err != nil {
		t.Fatalf("second StartSession after a crash must succeed: %v", err)
	}
	waitEvent(t, ws2, grt.EventStatus, 5*time.Second)
	if err := ws2.Close(context.Background()); err != nil {
		t.Fatalf("close second session: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Abort
// ---------------------------------------------------------------------------

func TestSupervisor_AbortReachesWorker(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	t.Setenv("GW_TESTWORKER_LOG", logPath)
	sup := newTestSupervisor(t, nil)

	ws, err := sup.StartSession(context.Background(), testRequest("abort-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second) // "started"

	if err := ws.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if event := waitEvent(t, ws, grt.EventStatus, 5*time.Second); event.Status != "aborted" {
		t.Fatalf("abort status = %#v", event)
	}
	data, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read worker log: %v", rerr)
	}
	if !strings.Contains(string(data), "abort") {
		t.Errorf("worker log missing abort RPC:\n%s", data)
	}
	if strings.Contains(string(data), "close") {
		t.Errorf("abort unexpectedly closed worker:\n%s", data)
	}
	pid := ws.outerPID
	if !pidAlive(pid) {
		t.Fatal("persistent_process worker exited after turn abort")
	}
	if err := ws.Send(context.Background(), grt.Input{Messages: []grt.Message{{Role: "user", Content: "after"}}}); err != nil {
		t.Fatalf("Send after abort: %v", err)
	}
	if event := waitEvent(t, ws, grt.EventText, 5*time.Second); event.Text != "echo:after" {
		t.Fatalf("post-abort text = %#v", event)
	}
	waitEvent(t, ws, grt.EventFinish, 5*time.Second)
	if ws.outerPID != pid || !pidAlive(pid) {
		t.Fatal("post-abort turn did not reuse the same worker PID")
	}
}

// ---------------------------------------------------------------------------
// Shutdown escalation
// ---------------------------------------------------------------------------

func TestSupervisor_CloseTimeoutEscalatesToSIGKILL(t *testing.T) {
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "ignore-close")
	sup := newTestSupervisor(t, func(c *Config) { c.StopGracePeriod = 1 * time.Second })

	ws, err := sup.StartSession(context.Background(), testRequest("stubborn-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)

	start := time.Now()
	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// SIGTERM is ignored by the stub; the supervisor must wait the grace
	// period and then SIGKILL the process group.
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("Close returned too fast (%v); expected SIGTERM grace then SIGKILL", elapsed)
	}
	if pidAlive(ws.outerPID) {
		t.Errorf("stubborn worker pid %d still alive after Close", ws.outerPID)
	}
	waitEventsClosed(t, ws, 5*time.Second)
}

// ---------------------------------------------------------------------------
// nsjail wrapper crash: the whole process group must be reaped
// ---------------------------------------------------------------------------

func TestSupervisor_NsjailWrapperCrashReapsGroup(t *testing.T) {
	fakeNsjail := writeFakeNsjail(t, fakeNsjailWrapper)
	sup := newTestSupervisor(t, func(c *Config) {
		c.Mode = config.ModeDev
		c.Isolation.Required = true
		c.Isolation.BinaryPath = fakeNsjail
	}, WithSpawner(func(ctx context.Context, spec spawnSpec) (*exec.Cmd, error) {
		return startCommand(ctx, buildNsjailCommand(spec.nsjailBinary, spec.profilePath, spec.workerExe, spec.workerArgs), spec.env, spec.logPath)
	}))

	ws, err := sup.StartSession(context.Background(), testRequest("wrap-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)
	if ws.outerPID == ws.workerPID {
		t.Fatal("fake nsjail wrapper must be a distinct PID from the worker")
	}
	if ws.workerPID <= 0 {
		t.Fatal("worker PID was not recorded during the handshake")
	}

	// Kill the nsjail wrapper; the worker is orphaned. The supervisor's
	// monitor must detect the wrapper exit and kill the whole group.
	if err := killProcess(ws.outerPID); err != nil {
		t.Fatalf("kill nsjail wrapper: %v", err)
	}
	waitEventsClosed(t, ws, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(ws.workerPID) {
		if time.Now().After(deadline) {
			t.Fatalf("orphaned worker pid %d survived the nsjail wrapper crash", ws.workerPID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	ws.mu.Lock()
	termErr := ws.err
	ws.mu.Unlock()
	if termErr == nil {
		t.Error("session must record the wrapper-crash terminal error")
	}
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("wrapper-crash session must be removed, got %d", n)
	}
}

// TestSupervisor_SocketPathUnderLimit guards the Unix socket path-length bug:
// a long gateway session id used verbatim in the socket path exceeds the
// kernel limit (~104 bytes on darwin, ~108 on linux) and the worker then dies
// at listen time. Session dirs must be derived from a short deterministic
// token so the socket path always fits.
func TestSupervisor_SocketPathUnderLimit(t *testing.T) {
	sup := newTestSupervisor(t, nil)
	longID := "550e8400-e29b-41d4-a716-446655440000-0123456789abcdef0123456789abcdef"
	ws, err := sup.StartSession(context.Background(), testRequest(longID))
	if err != nil {
		t.Fatalf("StartSession with long session id: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)
	if len(ws.socketPath) > 100 {
		t.Errorf("socket path too long (%d chars, >100): %s", len(ws.socketPath), ws.socketPath)
	}
	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSupervisor_SessionIDReusableAfterClose guards persistent crash recovery
// session-id-reuse bug: a terminated session left its per-session socket dir
// (and the w.sock file inside it) behind, and net.Listen("unix", ...) fails
// with EADDRINUSE on a stale path — so a later StartSession with the SAME
// SessionID (same logical session, a NEW process
// per turn) died at bind time. The socket survives whenever the worker is
// killed hard (SIGKILL escalation, crash, wrapper death) — the graceful-close
// path unlinks it, the kill paths do not — so terminate must remove the
// socket dir on every terminal path. The stub runs with "ignore-close" so the
// first Close escalates to SIGKILL and leaves the stale socket behind.
func TestSupervisor_SessionIDReusableAfterClose(t *testing.T) {
	t.Setenv("GW_TESTWORKER_BEHAVIOR", "ignore-close")
	sup := newTestSupervisor(t, func(c *Config) { c.StopGracePeriod = 1 * time.Second })
	const sessionID = "resume-1"

	ws, err := sup.StartSession(context.Background(), testRequest(sessionID))
	if err != nil {
		t.Fatalf("first StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)
	if err := ws.Send(context.Background(), grt.Input{
		Messages: []grt.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(t, ws, grt.EventText, 5*time.Second); ev.Text != "echo:hello" {
		t.Errorf("text event = %q, want %q", ev.Text, "echo:hello")
	}
	waitEvent(t, ws, grt.EventFinish, 5*time.Second)
	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitEventsClosed(t, ws, 5*time.Second)
	if n := sup.SessionCount(); n != 0 {
		t.Errorf("sessions left in supervisor after close: %d", n)
	}

	// Recovery: the same SessionID starts a NEW process after
	// the previous one closed. The stale socket must not block the re-bind.
	ws2, err := sup.StartSession(context.Background(), testRequest(sessionID))
	if err != nil {
		t.Fatalf("second StartSession with the same SessionID after Close must succeed: %v", err)
	}
	waitEvent(t, ws2, grt.EventStatus, 5*time.Second)
	if err := ws2.Send(context.Background(), grt.Input{
		Messages: []grt.Message{{Role: "user", Content: "again"}},
	}); err != nil {
		t.Fatalf("Send on resumed session: %v", err)
	}
	if ev := waitEvent(t, ws2, grt.EventText, 5*time.Second); ev.Text != "echo:again" {
		t.Errorf("text event = %q, want %q", ev.Text, "echo:again")
	}
	waitEvent(t, ws2, grt.EventFinish, 5*time.Second)
	if err := ws2.Close(context.Background()); err != nil {
		t.Fatalf("Close resumed session: %v", err)
	}
}

func TestWorkerDonePublishesAfterDeregisterAndRuntimeCleanup(t *testing.T) {
	sessionDir := t.TempDir()
	socketDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	sup := &Supervisor{sessions: make(map[string]*workerSession)}
	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	ws := &workerSession{
		sup: sup, req: testRequest("publish-after-cleanup"), ctx: ctx, cancel: cancel,
		state: stateRunning, done: make(chan struct{}), sessionDir: sessionDir, socketDir: socketDir,
		releaseSlot: func() {
			close(releaseStarted)
			<-allowRelease
		},
	}
	sup.sessions[ws.req.SessionID] = ws
	go ws.terminate(errors.New("worker exited"))
	<-releaseStarted
	select {
	case <-ws.Done():
		t.Fatal("Done published before terminal cleanup completed")
	default:
	}
	close(allowRelease)
	select {
	case <-ws.Done():
	case <-time.After(time.Second):
		t.Fatal("Done was not published after cleanup")
	}
	if sup.SessionCount() != 0 {
		t.Fatal("session remains registered after Done")
	}
	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session dir still exists after Done: %v", err)
	}
	if _, err := os.Stat(socketDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket dir still exists after Done: %v", err)
	}
}

// TestTerminateWaitsForReapedBeforeDirCleanup_OB5 is a regression test for
// O-B5: terminate must wait for the process tree to be reaped before removing
// runtime directories, preventing a race where a still-dying worker holds the
// unix socket or worker.log.
func TestTerminateWaitsForReapedBeforeDirCleanup_OB5(t *testing.T) {
	sessionDir := t.TempDir()
	socketDir := t.TempDir()
	// Create a sentinel file to prove the directory is not removed prematurely.
	sentinel := filepath.Join(sessionDir, "alive")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sup := &Supervisor{sessions: make(map[string]*workerSession)}
	ws := &workerSession{
		sup: sup, req: testRequest("ob5-wait-reaped"), ctx: ctx, cancel: cancel,
		state: stateRunning, done: make(chan struct{}),
		reaped:     make(chan struct{}),
		sessionDir: sessionDir, socketDir: socketDir,
	}
	sup.sessions[ws.req.SessionID] = ws

	go ws.terminate(errors.New("test"))

	// Give terminate time to reach the reaped wait. killGroup is a no-op
	// (pgid=0) and removeSession/releaseSlot are instant, so after a brief
	// sleep terminate is blocked on <-ws.reaped.
	time.Sleep(100 * time.Millisecond)

	// Directories must still exist while reaped is not closed.
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("session dir removed before process was reaped: %v", err)
	}

	// Simulate the process exiting: monitor closes reaped.
	close(ws.reaped)

	select {
	case <-ws.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("terminate did not complete after reaped was closed")
	}

	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session dir still exists after terminate: %v", err)
	}
	if _, err := os.Stat(socketDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket dir still exists after terminate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Session bookkeeping and validation
// ---------------------------------------------------------------------------

func TestSupervisor_DuplicateSessionRejected(t *testing.T) {
	sup := newTestSupervisor(t, nil)
	ws, err := sup.StartSession(context.Background(), testRequest("dup-1"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer ws.Close(context.Background())
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)

	_, err = sup.StartSession(context.Background(), testRequest("dup-1"))
	if err == nil {
		t.Fatal("second StartSession with the same id must fail")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("unexpected duplicate error: %v", err)
	}
}

func TestSupervisor_SendAfterCloseFails(t *testing.T) {
	sup := newTestSupervisor(t, nil)
	ws, err := sup.StartSession(context.Background(), testRequest("s-close"))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitEvent(t, ws, grt.EventStatus, 5*time.Second)
	if err := ws.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ws.Send(context.Background(), grt.Input{}); err == nil {
		t.Fatal("Send after Close must fail")
	}
}

func TestSupervisor_StartValidation(t *testing.T) {
	sup := newTestSupervisor(t, nil)
	cases := map[string]grt.StartRequest{
		"empty session":   {ModelID: "m", CallerID: "o", WorkspaceID: "w"},
		"empty model":     {SessionID: "s", CallerID: "o", WorkspaceID: "w"},
		"empty owner":     {SessionID: "s", ModelID: "m", WorkspaceID: "w"},
		"empty workspace": {SessionID: "s", ModelID: "m", CallerID: "o"},
	}
	for name, req := range cases {
		if _, err := sup.StartSession(context.Background(), req); err == nil {
			t.Errorf("%s: StartSession must fail validation", name)
		}
	}
}

func TestSupervisor_ConfigRejectsIsolationOffOutsideTest(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = config.ModeDev
	cfg.Isolation.Required = false
	cfg.WorkspaceRoot = t.TempDir()
	cfg.RuntimeDir = testRuntimeDir(t)
	cfg.WorkerExec = stubWorkerBin
	if _, err := NewSupervisor(cfg); err == nil {
		t.Fatal("dev mode with isolation.required=false must be rejected")
	}
}

// ---------------------------------------------------------------------------
// LocalExecutionBackend (the API layer's only execution path)
// ---------------------------------------------------------------------------

func TestLocalExecutionBackend_StartAndDrive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = config.ModeTest
	cfg.Isolation = config.IsolationConfig{
		Required: false,
		Mounts: config.MountsConfig{
			WorkspaceDir: "/workspace",
			AgentHomeDir: "/agent-home",
			TmpDir:       "/tmp",
		},
	}
	cfg.WorkspaceRoot = t.TempDir()
	cfg.RuntimeDir = testRuntimeDir(t)
	cfg.WorkerExec = stubWorkerBin
	cfg.StartTimeout = 5 * time.Second
	cfg.StopGracePeriod = 2 * time.Second

	backend, err := NewLocalExecutionBackend(cfg)
	if err != nil {
		t.Fatalf("NewLocalExecutionBackend: %v", err)
	}

	// Test mode: preflight trivially passes.
	if err := backend.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight in test mode must pass: %v", err)
	}

	handle, err := backend.Start(context.Background(), testRequest("be-1"))
	if err != nil {
		t.Fatalf("backend.Start: %v", err)
	}
	// handle is the canonical grt.ExecutionHandle; drive it through the
	// interface only.
	deadline := time.After(5 * time.Second)
	gotStarted := false
events:
	for {
		select {
		case ev, ok := <-handle.Events():
			if !ok {
				t.Fatal("events closed before 'started'")
			}
			if ev.Type == grt.EventStatus && ev.Status == "started" {
				gotStarted = true
				break events
			}
		case <-deadline:
			t.Fatal("timed out waiting for started")
		}
	}
	if !gotStarted {
		t.Fatal("did not observe session start")
	}
	if err := handle.Send(context.Background(), grt.Input{
		Messages: []grt.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("handle.Send: %v", err)
	}
	if err := handle.Abort(context.Background()); err != nil {
		t.Fatalf("handle.Abort: %v", err)
	}
	if err := handle.Close(context.Background()); err != nil {
		t.Fatalf("handle.Close: %v", err)
	}
}

func TestLocalExecutionBackend_PreflightFailClosed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = config.ModeDev
	cfg.Isolation = config.IsolationConfig{Required: true, BinaryPath: filepath.Join(t.TempDir(), "missing"), CloneNewPID: true}
	cfg.WorkspaceRoot = t.TempDir()
	cfg.RuntimeDir = testRuntimeDir(t)
	cfg.WorkerExec = stubWorkerBin
	backend, err := NewLocalExecutionBackend(cfg)
	if err != nil {
		t.Fatalf("NewLocalExecutionBackend: %v", err)
	}
	if err := backend.Preflight(context.Background()); err == nil {
		t.Fatal("Preflight must fail closed when the nsjail binary is missing")
	}
}
