package nsjail

import (
	"os"
	"path/filepath"
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
		"clone_newpid: false;",
		"clone_newnet: false;",
		`uidmap: { inside_id: "1000"; outside_id: "1000"; count: "1"; };`,
		`gidmap: { inside_id: "1000"; outside_id: "1000"; count: "1"; };`,
		"user: \"1000\";",
		"rlimit_nofile: 1024;",
		"rlimit_nproc: 256;",
		`mount: { src: "` + layout.WorkspaceDir + `"; dst: "/workspace"; is_bind: true; rw: true; mandatory: true; };`,
		`mount: { src: "` + layout.AgentHomeDir + `"; dst: "/agent-home"; is_bind: true; rw: true; mandatory: true; };`,
		`mount: { src: "` + layout.SocketDir + `"; dst: "` + layout.SocketDir + `"; is_bind: true; rw: true; mandatory: true; };`,
		`tmpfs: { dst: "/tmp"; rw: true; };`,
		"seccomp_string: \"POLICY x86_64 {",
		`keep_env: "GW_WORKER_SOCKET";`,
		`env: { key: "HOME"; value: "/agent-home"; };`,
		`cwd: "/workspace";`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("profile missing %q", want)
		}
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
	// Exactly three bind mounts (workspace, agent-home, socket dir); /tmp is a
	// tmpfs, not a bind mount.
	if got := strings.Count(c, "is_bind: true"); got != 3 {
		t.Errorf("bind mounts = %d, want exactly 3 (workspace, agent-home, socket dir)", got)
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
	if !strings.Contains(c, `tmpfs: { dst: "/var/tmp"; rw: true; };`) {
		t.Errorf("custom tmpfs missing: %s", c)
	}
	if strings.Contains(c, "rlimit_nofile") {
		t.Error("zero rlimits must emit no rlimit lines")
	}
	if !strings.Contains(c, `uidmap: { inside_id: "4242"; outside_id: "4242"; count: "1"; };`) {
		t.Errorf("custom uid mapping missing: %s", c)
	}
	if !strings.Contains(c, `user: "4242";`) {
		t.Errorf("custom user missing: %s", c)
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
