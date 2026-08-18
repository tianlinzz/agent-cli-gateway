package nsjail

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// testLayout builds a valid SessionLayout rooted in a temp dir.
func testLayout(t *testing.T) SessionLayout {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	home := filepath.Join(root, "home")
	sock := filepath.Join(root, "sock")
	for _, d := range []string{ws, home, sock} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return SessionLayout{
		WorkspaceDir: ws,
		AgentHomeDir: home,
		SocketDir:    sock,
	}
}

func TestBuildProfile_KafelDefaults(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	layout := testLayout(t)

	p, err := Build(iso, layout, "sess-1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	c := p.Config
	for _, want := range []string{
		`name: "agent-session-sess-1";`,
		"mode: ONCE;",
		"time_limit: 0;",
		"clone_newns: true;",
		"clone_newroot: true;",
		"clone_newpid: true;",
		"clone_newnet: false;",
		`uidmap: { inside_id: "65532"; outside_id: "65532"; count: 1; };`,
		`gidmap: { inside_id: "65532"; outside_id: "65532"; count: 1; };`,
		"rlimit_nofile: 1024;",
		"rlimit_nproc: 256;",
		`mount: { src: "` + layout.WorkspaceDir + `"; dst: "/workspace"; is_bind: true; rw: true; mandatory: true; };`,
		`mount: { src: "` + layout.AgentHomeDir + `"; dst: "/home/agent"; is_bind: true; rw: true; mandatory: true; };`,
		`mount: { src: "` + layout.SocketDir + `"; dst: "` + layout.SocketDir + `"; is_bind: true; rw: true; mandatory: true; };`,
		`mount: { dst: "/tmp"; fstype: "tmpfs"; options: "size=256m"; rw: true; mandatory: true; };`,
		`mount: { dst: "/proc"; fstype: "proc"; rw: false; mandatory: true; } ;`,
		`keep_env: "GW_WORKER_SOCKET";`,
		`keep_env: "GW_WORKER_SESSION_ID";`,
		`keep_env: "GW_WORKSPACE_DIR";`,
		`keep_env: "GW_AGENT_HOME";`,
		`env: { key: "HOME"; value: "/home/agent"; };`,
		`cwd: "/workspace";`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("profile missing %q", want)
		}
	}

	// The seccomp policy must match the compiled binary's architecture (O-F06).
	seccompArch := "x86_64"
	if runtime.GOARCH == "arm64" {
		seccompArch = "aarch64"
	}
	if !strings.Contains(c, "seccomp_string: \"POLICY "+seccompArch+" {") {
		t.Errorf("profile seccomp policy must match host arch %s:\n%s", seccompArch, c)
	}

	if !strings.Contains(c, "execve") || !strings.Contains(c, "socket") || !strings.Contains(c, "connect") {
		t.Error("kafel policy must whitelist execve and network syscalls (provider reachability)")
	}
	if p.Name != "agent-session-sess-1" {
		t.Errorf("Profile.Name = %q, want %q", p.Name, "agent-session-sess-1")
	}
}

// TestBuildProfile_MountsScopedToThisSession guards the phase-1 per-session
// mount boundary: the profile must bind-mount only THIS session's workspace,
// agent-home, and socket dir — never sibling sessions' dirs (e.g. another
// session's agent-home under sessions/ or another session's socket dir under
// sockets/). The mount namespace hides sibling sockets/agent-homes from the
// jail's primary view; the profile must not widen that to the whole runtime
// dir.
func TestBuildProfile_MountsScopedToThisSession(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	root := t.TempDir()

	ws := filepath.Join(root, "ws")
	thisHome := filepath.Join(root, "sessions", "aaaa", "agent-home")
	thisSocketDir := filepath.Join(root, "sockets", "aaaa")
	// Sibling session state that must NOT leak into this jail's profile.
	sibHome := filepath.Join(root, "sessions", "bbbb", "agent-home")
	sibSocketDir := filepath.Join(root, "sockets", "bbbb")
	for _, d := range []string{ws, thisHome, thisSocketDir, sibHome, sibSocketDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	p, err := Build(iso, SessionLayout{
		WorkspaceDir: ws,
		AgentHomeDir: thisHome,
		SocketDir:    thisSocketDir,
	}, "this-session")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := p.Config

	// This session's paths must be bound.
	for _, want := range []string{ws, thisHome, thisSocketDir} {
		if !strings.Contains(c, want) {
			t.Errorf("profile must bind-mount this session's %q", want)
		}
	}
	// Sibling session dirs must never be referenced.
	for _, bad := range []string{sibHome, sibSocketDir} {
		if strings.Contains(c, bad) {
			t.Errorf("profile must not mount sibling session dir %q", bad)
		}
	}
	// The private root also exposes only read-only runtime libraries; session
	// mounts remain the only writable bind mounts.
	if got := strings.Count(c, "is_bind: true"); got < 3 {
		t.Errorf("bind mounts = %d, want at least the three session mounts", got)
	}
	if !strings.Contains(c, "clone_newroot: true") {
		t.Fatal("profile must create a private root filesystem")
	}
}

// TestBuildProfile_KeepEnvCoversWorkerPlacementVars is the regression test for
// the missing-keep_env bug: the supervisor passes the worker its placement
// (GW_WORKSPACE_DIR, GW_AGENT_HOME) and identity (GW_WORKER_SOCKET,
// GW_WORKER_SESSION_ID) through the process environment, and nsjail's
// keep_env drops every unlisted variable — so the default profile must list
// all four or the jailed worker reads them as empty (empty WorkspaceDir /
// AgentHome in the adapter config, and sandbox_capabilities losing its
// "nsjail" marker which is keyed off GW_AGENT_HOME).
func TestBuildProfile_KeepEnvCoversWorkerPlacementVars(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	p, err := Build(iso, testLayout(t), "sess-keepenv")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, v := range []string{"GW_WORKER_SOCKET", "GW_WORKER_SESSION_ID", "GW_WORKSPACE_DIR", "GW_AGENT_HOME"} {
		if want := `keep_env: "` + v + `";`; !strings.Contains(p.Config, want) {
			t.Errorf("profile missing %s (jailed worker would read %s as empty)", want, v)
		}
	}
}

func TestBuildProfile_CustomMountDirs(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	iso.Mounts.WorkspaceDir = "/srv/ws"
	iso.Mounts.AgentHomeDir = "/var/agent-home"
	iso.Mounts.TmpDir = "/var/tmp"
	iso.Rlimits = config.RlimitsConfig{} // no rlimits set
	iso.UserNamespace = config.UserNamespaceConfig{Enabled: true, UID: 4242, GID: 4343}
	layout := testLayout(t)

	p, err := Build(iso, layout, "sess-2")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := p.Config
	if !strings.Contains(c, `dst: "/srv/ws";`) {
		t.Errorf("custom workspace mount missing: %s", c)
	}
	if !strings.Contains(c, `dst: "/var/agent-home";`) {
		t.Errorf("custom agent home mount missing: %s", c)
	}
	if !strings.Contains(c, `mount: { dst: "/var/tmp"; fstype: "tmpfs"; options: "size=256m"; rw: true; mandatory: true; };`) {
		t.Errorf("custom tmpfs missing: %s", c)
	}
	if strings.Contains(c, "rlimit_nofile") {
		t.Error("zero rlimits must emit no rlimit lines")
	}
	if !strings.Contains(c, `uidmap: { inside_id: "4242"; outside_id: "4242"; count: 1; };`) {
		t.Errorf("custom uid mapping missing: %s", c)
	}
	if !strings.Contains(c, `env: { key: "HOME"; value: "/var/agent-home"; };`) {
		t.Errorf("HOME env must point at the agent home: %s", c)
	}
}

func TestBuildProfile_NetworkNamespaceOptIn(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	iso.NetworkNamespace = true
	layout := testLayout(t)
	p, err := Build(iso, layout, "sess-3")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(p.Config, "clone_newnet: true;") {
		t.Errorf("network namespace opt-in must set clone_newnet: true:\n%s", p.Config)
	}
}

// TestBuildProfile_CloneNewPIDOptOut verifies the fallback switch: with
// CloneNewPID=false the PID namespace stays shared and no /proc pseudo-fs is
// mounted.
func TestBuildProfile_CloneNewPIDOptOut(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	iso.CloneNewPID = false
	layout := testLayout(t)
	p, err := Build(iso, layout, "sess-nopid")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(p.Config, "clone_newpid: false;") {
		t.Errorf("CloneNewPID=false must emit clone_newpid: false:\n%s", p.Config)
	}
	if strings.Contains(p.Config, `dst: "/proc"; fstype: "proc"`) {
		t.Errorf("no /proc proc mount should be emitted when the PID namespace is shared:\n%s", p.Config)
	}
}

// TestBuildProfile_EtcMountNarrowed is a regression test for the /etc
// exposure-surface finding (O-C4): the profile used to bind-mount the whole
// /etc directory, exposing host world-readable files. It must now mount only
// the minimal per-file read-only set the CLIs need.
func TestBuildProfile_EtcMountNarrowed(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	layout := testLayout(t)
	p, err := Build(iso, layout, "sess-etc")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := p.Config
	if strings.Contains(c, `src: "/etc";`) {
		t.Errorf("profile must not bind-mount the whole /etc directory:\n%s", c)
	}
	// Each surviving minimal file must be mounted individually, mirroring the
	// Build os.Stat guard (a file absent on this host is skipped).
	for _, f := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/passwd", "/etc/group", "/etc/ssl/certs"} {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		want := fmt.Sprintf(`src: %q; dst: %q;`, f, f)
		if !strings.Contains(c, want) {
			t.Errorf("profile must bind-mount existing narrow file %q", f)
		}
	}
}

func TestBuildProfile_SeccompOffOmitted(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	iso.Seccomp.Policy = config.SeccompOff
	layout := testLayout(t)
	p, err := Build(iso, layout, "sess-4")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(p.Config, "seccomp_string") {
		t.Errorf("seccomp off must omit the kafel policy:\n%s", p.Config)
	}
}

func TestBuildProfile_InvalidSeccompPolicyFailsClosed(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	iso.Seccomp.Policy = "banana"
	layout := testLayout(t)
	if _, err := Build(iso, layout, "sess-5"); err == nil {
		t.Fatal("Build must reject an unknown seccomp policy")
	}
}

// TestKafelPolicyForArch verifies the arch-aware seccomp policy (O-F06):
// amd64 -> x86_64, arm64 -> aarch64 (x86-only syscalls dropped), and any other
// architecture fails closed.
func TestKafelPolicyForArch(t *testing.T) {
	x86, err := kafelPolicyForArch("amd64")
	if err != nil {
		t.Fatalf("amd64 policy: %v", err)
	}
	if !strings.Contains(x86, "POLICY x86_64 {") {
		t.Errorf("amd64 policy must declare x86_64")
	}
	if !strings.Contains(x86, "arch_prctl") || !strings.Contains(x86, "mmap2") {
		t.Error("x86_64 policy must keep x86-only syscalls")
	}

	arm, err := kafelPolicyForArch("arm64")
	if err != nil {
		t.Fatalf("arm64 policy: %v", err)
	}
	if !strings.Contains(arm, "POLICY aarch64 {") {
		t.Errorf("arm64 policy must declare aarch64")
	}
	for _, dropped := range []string{"arch_prctl", "mmap2", "stat64", "ugetrlimit", "getuid32"} {
		if strings.Contains(arm, dropped) {
			t.Errorf("aarch64 policy must not allow x86-only syscall %q", dropped)
		}
	}
	for _, kept := range []string{"openat", "execve", "clone3", "socket", "connect"} {
		if !strings.Contains(arm, kept) {
			t.Errorf("aarch64 policy must keep common syscall %q", kept)
		}
	}

	if _, err := kafelPolicyForArch("riscv64"); err == nil {
		t.Fatal("unsupported architecture must fail closed")
	}
}

func TestBuildProfile_FailClosed(t *testing.T) {
	iso := config.DefaultGatewayConfig().Isolation
	layout := testLayout(t)

	tests := []struct {
		name   string
		sessID string
		mutate func(*SessionLayout)
	}{
		{"empty session id", "", nil},
		{"relative workspace dir", "s", func(l *SessionLayout) { l.WorkspaceDir = "relative/ws" }},
		{"empty workspace dir", "s", func(l *SessionLayout) { l.WorkspaceDir = "" }},
		{"missing workspace dir", "s", func(l *SessionLayout) { l.WorkspaceDir = filepath.Join(t.TempDir(), "does-not-exist") }},
		{"empty agent home", "s", func(l *SessionLayout) { l.AgentHomeDir = "" }},
		{"relative agent home", "s", func(l *SessionLayout) { l.AgentHomeDir = "rel/home" }},
		{"empty socket dir", "s", func(l *SessionLayout) { l.SocketDir = "" }},
		{"relative socket dir", "s", func(l *SessionLayout) { l.SocketDir = "rel/sock" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := layout
			if tt.mutate != nil {
				tt.mutate(&l)
			}
			if _, err := Build(iso, l, tt.sessID); err == nil {
				t.Errorf("Build must fail closed for %q", tt.name)
			}
		})
	}
}

func TestValidateBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "nsjail")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nonExe := filepath.Join(dir, "nsjail-noexec")
	if err := os.WriteFile(nonExe, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"empty path", "", false},
		{"missing binary", filepath.Join(dir, "nope"), false},
		{"directory", dir, false},
		{"non-executable file", nonExe, false},
		{"executable file", exe, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBinary(tt.path)
			if tt.want && err != nil {
				t.Fatalf("ValidateBinary(%q) = %v, want nil", tt.path, err)
			}
			if !tt.want && err == nil {
				t.Fatalf("ValidateBinary(%q) = nil, want error", tt.path)
			}
		})
	}
}

// TestBuild_TmpfsSizeAndFsizeConfigurable verifies the final-review resource
// knobs: the per-session tmpfs size is configurable (bounded /tmp per
// session) and RLIMIT_FSIZE bounds any single file written inside the jail.
func TestBuild_TmpfsSizeAndFsizeConfigurable(t *testing.T) {
	iso := config.IsolationConfig{
		Required:      true,
		CloneNewPID:   true,
		UserNamespace: config.UserNamespaceConfig{Enabled: true, UID: 1000, GID: 1000},
		Rlimits: config.RlimitsConfig{
			MaxOpenFiles: 1024,
			MaxProcesses: 256,
			MaxFileBytes: 64 << 20,
		},
		Mounts: config.MountsConfig{
			WorkspaceDir: "/workspace",
			AgentHomeDir: "/agent-home",
			TmpDir:       "/tmp",
			TmpfsSizeMiB: 64,
		},
	}
	root := t.TempDir()
	for _, d := range []string{"ws", "home", "sock"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prof, err := Build(iso, SessionLayout{
		WorkspaceDir: filepath.Join(root, "ws"),
		AgentHomeDir: filepath.Join(root, "home"),
		SocketDir:    filepath.Join(root, "sock"),
	}, "sess-tmpfs")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prof.Config, `options: "size=64m"`) {
		t.Errorf("tmpfs size not configurable:\n%s", prof.Config)
	}
	if !strings.Contains(prof.Config, "rlimit_fsize: 67108864;") {
		t.Errorf("rlimit_fsize missing:\n%s", prof.Config)
	}
}
