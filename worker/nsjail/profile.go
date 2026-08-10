// Package nsjail assembles the nsjail sandbox configuration for one session
// and provides the fail-closed binary checks used by the worker supervisor.
//
// The profile builder is PURE DATA ASSEMBLY: it never touches the filesystem
// and never executes anything, so it is cross-platform and unit-testable on
// any host (including darwin, where a real nsjail binary cannot run). Only the
// preflight that runs a real minimal jail lives behind the linux build tag.
package nsjail

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// SessionLayout describes the real host paths and in-jail mount points for one
// session. All host paths must be absolute.
type SessionLayout struct {
	// WorkspaceDir is the real controlled directory (resolved from the opaque
	// workspace_id by the workspace resolver) bind-mounted at
	// Isolation.Mounts.WorkspaceDir. It must be a directory controlled by the
	// gateway, never a client-injected path.
	WorkspaceDir string
	// AgentHomeDir is the per-session writable host directory bind-mounted at
	// Isolation.Mounts.AgentHomeDir.
	AgentHomeDir string
	// SocketDir is the per-session socket directory. It is bind-mounted at the
	// same path inside the jail so the worker and the supervisor share one
	// identical socket path.
	SocketDir string
	// KeepEnv lists environment variables passed through to the jailed worker.
	// When empty the defaults (PATH, HOME, GW_WORKER_SOCKET,
	// GW_WORKER_SESSION_ID) are used.
	KeepEnv []string
}

// Profile is the assembled nsjail configuration for one session.
type Profile struct {
	// Name is the jail name (derived from the session identifier).
	Name string
	// Config is the complete nsjail config file text.
	Config string
}

// defaultKeepEnv mirrors the env vars the worker supervisor relies on plus the
// basics a CLI needs.
var defaultKeepEnv = []string{"PATH", "HOME", "GW_WORKER_SOCKET", "GW_WORKER_SESSION_ID"}

// Build assembles an nsjail config file for one session from the isolation
// config and the session's real host paths. It is fail-closed: mount sources
// must be non-empty absolute paths, the user-namespace mapping must be present
// when enabled, and the seccomp policy must be "kafel" (inlined whitelist) or
// "off" (test profile only).
//
// The profile uses a private mount namespace and a new root filesystem. Only
// the read-only runtime directories needed by the worker/CLI are exposed; the
// caller workspace, agent home, socket, and tmp are per-session mounts.
//
// The resulting config drives `nsjail -Mo --config <file> -- <worker>`:
// workspace and agent-home are real bind mounts (persistence across turns),
// /tmp is a per-session tmpfs, the socket dir is bind-mounted at the same path
// so the worker uses one identical socket path, and the jail runs unprivileged
// in a user namespace with no CAP_SYS_ADMIN.
func Build(iso config.IsolationConfig, layout SessionLayout, sessionID string) (Profile, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Profile{}, fmt.Errorf("nsjail: build profile: empty session id")
	}
	if err := validateLayout(layout); err != nil {
		return Profile{}, fmt.Errorf("nsjail: build profile: %w", err)
	}

	mounts := iso.Mounts
	if strings.TrimSpace(mounts.WorkspaceDir) == "" {
		mounts.WorkspaceDir = "/workspace"
	}
	if strings.TrimSpace(mounts.AgentHomeDir) == "" {
		mounts.AgentHomeDir = "/agent-home"
	}
	if strings.TrimSpace(mounts.TmpDir) == "" {
		mounts.TmpDir = "/tmp"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "name: %q;\n", "agent-session-"+sessionID)
	b.WriteString("\n")
	b.WriteString("mode: ONCE;\n")
	b.WriteString("time_limit: 0;\n")
	b.WriteString("\n")
	b.WriteString("hostname: \"agent\";\n")
	b.WriteString("max_conns_per_ip: 0;\n")
	b.WriteString("\n")
	// Namespaces. Phase 1: no separate network namespace (agents must reach
	// their providers; egress is controlled by the container), and the PID
	// namespace stays shared so the worker PID observed by the supervisor is
	// the host PID.
	b.WriteString("clone_newns: true;\n")
	b.WriteString("clone_newroot: true;\n")
	b.WriteString("clone_newpid: false;\n")
	b.WriteString("clone_newipc: true;\n")
	b.WriteString("clone_newuts: true;\n")
	if iso.NetworkNamespace {
		b.WriteString("clone_newnet: true;\n")
	} else {
		b.WriteString("clone_newnet: false;\n")
	}
	b.WriteString("\n")

	if iso.UserNamespace.Enabled {
		fmt.Fprintf(&b, "uidmap: { inside_id: %q; outside_id: %q; count: 1; };\n", itoa(iso.UserNamespace.UID), itoa(iso.UserNamespace.UID))
		fmt.Fprintf(&b, "gidmap: { inside_id: %q; outside_id: %q; count: 1; };\n", itoa(iso.UserNamespace.GID), itoa(iso.UserNamespace.GID))
		b.WriteString("\n")
	}

	writeRlimit(&b, "rlimit_nofile", int64(iso.Rlimits.MaxOpenFiles), 0)
	writeRlimit(&b, "rlimit_nproc", int64(iso.Rlimits.MaxProcesses), 0)
	writeRlimit(&b, "rlimit_core", iso.Rlimits.MaxCoreDumpBytes, -1)
	writeRlimit(&b, "rlimit_as", iso.Rlimits.MaxAddressSpaceBytes, -1)
	b.WriteString("\n")

	writeMount(&b, layout.WorkspaceDir, mounts.WorkspaceDir)
	writeMount(&b, layout.AgentHomeDir, mounts.AgentHomeDir)
	writeMount(&b, layout.SocketDir, layout.SocketDir)
	// Runtime dependencies are read-only mounts into the private root. No
	// gateway runtime/config/workspace parent is mounted, so sibling sessions
	// and gateway secrets remain outside the jail view.
	for _, dir := range []string{"/bin", "/usr", "/usr/local", "/lib", "/etc"} {
		if _, err := os.Stat(dir); err == nil {
			fmt.Fprintf(&b, "mount: { src: %q; dst: %q; is_bind: true; rw: false; mandatory: true; } ;\n", dir, dir)
		}
	}
	fmt.Fprintf(&b, "mount: { dst: %q; fstype: \"tmpfs\"; options: \"size=256m\"; rw: true; mandatory: true; };\n", mounts.TmpDir)
	b.WriteString("\n")

	switch iso.Seccomp.Policy {
	case config.SeccompKafel, "":
		b.WriteString("seccomp_string: ")
		b.WriteString(strconv.Quote(kafelPolicy))
		b.WriteString(";\n")
	case config.SeccompOff:
		// Only valid in the test profile; no seccomp filter applied.
	default:
		return Profile{}, fmt.Errorf("nsjail: build profile: seccomp policy %q invalid", iso.Seccomp.Policy)
	}
	b.WriteString("\n")

	keepEnv := layout.KeepEnv
	if len(keepEnv) == 0 {
		keepEnv = defaultKeepEnv
	}
	for _, k := range dedupe(keepEnv) {
		fmt.Fprintf(&b, "keep_env: %q;\n", k)
	}
	// HOME points at the per-session agent home inside the jail.
	fmt.Fprintf(&b, "env: { key: \"HOME\"; value: %q; };\n", mounts.AgentHomeDir)
	fmt.Fprintf(&b, "env: { key: \"TMPDIR\"; value: %q; };\n", mounts.TmpDir)
	fmt.Fprintf(&b, "cwd: %q;\n", mounts.WorkspaceDir)

	return Profile{
		Name:   "agent-session-" + sessionID,
		Config: b.String(),
	}, nil
}

// ValidateBinary is the cross-platform fail-closed guard used before any
// spawn: the configured nsjail binary must exist, be a regular file, and be
// executable. It runs on every platform (including darwin) so a missing or
// non-executable nsjail can never be silently bypassed.
func ValidateBinary(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("nsjail: binary path is empty while isolation is required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("nsjail: binary %q: %w", path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("nsjail: binary %q is a directory, not an executable", path)
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("nsjail: binary %q is not executable", path)
	}
	return nil
}

// validateLayout fails closed when a mount source is missing, relative, or
// (for the workspace) fails to resolve inside a real directory.
func validateLayout(layout SessionLayout) error {
	required := []struct {
		name string
		path string
	}{
		{"workspace dir", layout.WorkspaceDir},
		{"agent home dir", layout.AgentHomeDir},
		{"socket dir", layout.SocketDir},
	}
	for _, r := range required {
		if strings.TrimSpace(r.path) == "" {
			return fmt.Errorf("%s must not be empty", r.name)
		}
		if !filepath.IsAbs(r.path) {
			return fmt.Errorf("%s must be absolute (got %q)", r.name, r.path)
		}
	}
	// The workspace dir must be a real directory: bind-mounting a
	// non-existent path would silently create a hollow mount point.
	st, err := os.Stat(layout.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("workspace dir %q: %w", layout.WorkspaceDir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("workspace dir %q is not a directory", layout.WorkspaceDir)
	}
	return nil
}

func writeMount(b *strings.Builder, src, dst string) {
	fmt.Fprintf(b, "mount: { src: %q; dst: %q; is_bind: true; rw: true; mandatory: true; };\n", src, dst)
}

func writeRlimit(b *strings.Builder, key string, value, unset int64) {
	if value != unset {
		fmt.Fprintf(b, "%s: %d;\n", key, value)
	}
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// kafelPolicy is the base seccomp whitelist. It is deliberately generous: the
// three first-generation agents run on modern Go runtimes and need the usual
// process/io/fs/network/time syscalls. The exact whitelist is validated
// against real agents on Linux CI (task 7) and refined there.
const kafelPolicy = `POLICY x86_64 {
    ALLOW {
        /* process */
        read, write, close, dup, dup2, dup3, fcntl, ioctl,
        readv, writev, pread64, pwrite64, preadv, pwritev,
        open, openat, openat2, creat, close_range, lseek,
        mmap, mmap2, munmap, mprotect, mremap, msync, brk, madvise,
        stat, lstat, fstat, newfstatat, statx, stat64, lstat64, fstat64,
        access, faccessat, faccessat2, readlink, readlinkat,
        unlink, unlinkat, mkdir, mkdirat, rmdir, rename, renameat, renameat2,
        chmod, fchmod, fchmodat, chown, fchown, lchown, fchownat,
        chown32, fchown32, truncate, ftruncate,
        link, linkat, symlink, symlinkat, getdents, getdents64,
        utimensat, futimesat, futimes, utime, utimes,
        exit, exit_group, fork, vfork, clone, clone3, execve, execveat,
        wait4, waitid, waitpid, kill, tkill, tgkill,
        rt_sigaction, rt_sigprocmask, rt_sigpending, rt_sigtimedwait,
        rt_sigqueueinfo, rt_sigsuspend, rt_sigreturn, sigaltstack,
        getpid, getppid, gettid, getsid, setsid, setpgid, getpgid,
        getuid, getgid, geteuid, getegid, getuid32, getgid32,
        geteuid32, getegid32, setuid, setgid, setreuid, setregid,
        setresuid, setresgid, setuid32, setgid32, setreuid32, setregid32,
        setresuid32, setresgid32, getgroups, getgroups32, setgroups,
        prctl, getrlimit, setrlimit, prlimit64, ugetrlimit, umask,
        uname, sched_yield, sched_getaffinity, sched_setaffinity,
        nanosleep, clock_nanosleep, clock_gettime, clock_getres,
        gettimeofday, time, times, getitimer, setitimer,
        futex, futex_waitv, getrandom, pipe, pipe2, socketpair,
        poll, ppoll, select, pselect6,
        epoll_create, epoll_create1, epoll_ctl, epoll_wait, epoll_pwait,
        eventfd, eventfd2, inotify_init, inotify_init1,
        inotify_add_watch, inotify_rm_watch,
        fdatasync, fsync, sync, syncfs, sendfile, copy_file_range,
        statfs, fstatfs, statfs64, fstatfs64,
        arch_prctl, set_tid_address, set_robust_list, rseq,
        getcpu, getcwd, chdir, fchdir, sysinfo, alarm,
        /* network (phase 1 keeps the provider reachable) */
        socket, bind, listen, accept, accept4, connect,
        getsockname, getpeername, getsockopt, setsockopt,
        sendto, recvfrom, sendmsg, recvmsg, sendmmsg, recvmmsg, shutdown,
        /* memory */
        mlock, munlock, mlockall, munlockall, mincore, mlock2
    }
}`
