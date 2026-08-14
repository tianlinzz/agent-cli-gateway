#!/bin/sh
#
# nsjail smoke validation for Linux CI. nsjail is Linux-only; this runs in the
# built image on Linux CI (NON-PRIVILEGED: --user 65532:65532 --cap-drop=ALL
# --security-opt=no-new-privileges), NOT on the darwin dev host.
#
# Validates:
#   1. `ldd` — every shared-library dependency resolves.
#   2. A minimal smoke jail (mode: ONCE, private mount + user + PID namespace)
#      executes a trivial command inside the jail as an unprivileged user.
#   3. Per-jail PID namespace: killing the nsjail wrapper reaps its entire
#      process tree (the O-A3 regression — a CLI that escaped into its own
#      process group still dies with the wrapper).
#
# Usage: docker/nsjail-smoke.sh   (must run inside the runtime image, or on a
# Linux host with nsjail on PATH).
set -eu

NSJAIL_BIN="${NSJAIL_BIN:-/usr/local/bin/nsjail}"

echo "== ldd =="
ldd "$NSJAIL_BIN"

echo "== minimal smoke jail (unprivileged, private user+mount+PID ns) =="
# nsjail -Mo: mode ONCE, one jail, exit when the command exits. The profile
# mirrors the gateway's generated profile: unprivileged user namespace
# (65532:65532, no CAP_SYS_ADMIN), a private PID namespace (clone_newpid), a
# namespaced /proc, and the narrowed read-only runtime mounts.
"$NSJAIL_BIN" -Mo --config /dev/stdin -- /bin/true <<'EOF'
mode: ONCE;
clone_newns: true;
clone_newpid: true;
clone_newipc: true;
clone_newuts: true;
clone_newnet: false;
uidmap: { inside_id: "65532"; outside_id: "65532"; count: 1; };
gidmap: { inside_id: "65532"; outside_id: "65532"; count: 1; };
mount: { dst: "/tmp"; fstype: "tmpfs"; options: "size=64m"; rw: true; mandatory: true; };
mount: { dst: "/proc"; fstype: "proc"; rw: false; mandatory: true; };
mount: { src: "/bin"; dst: "/bin"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/usr"; dst: "/usr"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/lib"; dst: "/lib"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/lib64"; dst: "/lib64"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/etc/resolv.conf"; dst: "/etc/resolv.conf"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/etc/passwd"; dst: "/etc/passwd"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/etc/group"; dst: "/etc/group"; is_bind: true; rw: false; mandatory: true; };
EOF

echo "== PID namespace: killing the wrapper reaps the whole tree =="
# Fork a jailed command that leaves a background sleeper running. Killing the
# nsjail wrapper (PID 1 inside the jail) must make the kernel SIGKILL every
# process in that namespace, including the background child.
SMOKE_DIR="$(mktemp -d)"
trap 'rm -rf "$SMOKE_DIR"' EXIT

cat > "$SMOKE_DIR/pidns.conf" <<'EOF'
mode: ONCE;
clone_newns: true;
clone_newpid: true;
clone_newipc: true;
clone_newuts: true;
clone_newnet: false;
uidmap: { inside_id: "65532"; outside_id: "65532"; count: 1; };
gidmap: { inside_id: "65532"; outside_id: "65532"; count: 1; };
mount: { dst: "/tmp"; fstype: "tmpfs"; options: "size=64m"; rw: true; mandatory: true; };
mount: { dst: "/proc"; fstype: "proc"; rw: false; mandatory: true; };
mount: { src: "/bin"; dst: "/bin"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/usr"; dst: "/usr"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/lib"; dst: "/lib"; is_bind: true; rw: false; mandatory: true; };
mount: { src: "/lib64"; dst: "/lib64"; is_bind: true; rw: false; mandatory: true; };
EOF

"$NSJAIL_BIN" -Mo --config "$SMOKE_DIR/pidns.conf" -- /bin/sh -c 'sleep2891 & exec sleep2891' &
WRAPPER_PID=$!

# Give the wrapper a moment to spawn its PID-1 tree, then kill it.
sleep 2
kill -9 "$WRAPPER_PID" 2>/dev/null || true
wait "$WRAPPER_PID" 2>/dev/null || true

if pgrep -f 'sleep2891' >/dev/null 2>&1; then
  echo "FAIL: a jailed process survived killing its nsjail wrapper" >&2
  exit 1
fi

echo "OK: nsjail smoke validation passed"
