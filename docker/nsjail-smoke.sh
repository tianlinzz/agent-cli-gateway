#!/bin/sh
#
# nsjail smoke validation for Linux CI. nsjail is Linux-only; this runs in the
# built image on Linux CI (NON-PRIVILEGED: --user 65532:65532 --cap-drop=ALL
# --security-opt=no-new-privileges), NOT on the darwin dev host.
#
# Validates:
#   1. `ldd` — every shared-library dependency resolves.
#   2. The REAL generated profile (via `gateway -print-nsjail-profile`, which
#      exercises worker/nsjail.Build including the arch-selected seccomp policy,
#      the per-jail PID namespace, and the narrowed mounts) runs a minimal jail.
#   3. Per-jail PID namespace: killing the nsjail wrapper reaps its entire
#      process tree (the O-A3 regression — a CLI that escaped into its own
#      process group still dies with the wrapper).
#   4. keep_env: GW_WORKSPACE_DIR/GW_AGENT_HOME are listed in the profile and
#      verifiably survive into the jailed process.
#
# Usage: docker/nsjail-smoke.sh   (must run inside the runtime image, or on a
# Linux host with nsjail + gateway on PATH).
set -eu

NSJAIL_BIN="${NSJAIL_BIN:-/usr/local/bin/nsjail}"
GATEWAY_BIN="${GATEWAY_BIN:-/usr/local/bin/gateway}"

SMOKE_DIR="$(mktemp -d)"
trap 'rm -rf "$SMOKE_DIR"' EXIT

echo "== ldd =="
ldd "$NSJAIL_BIN"

echo "== generate the real nsjail profile =="
# Emit the exact profile the supervisor will run (same worker/nsjail.Build,
# arch-selected seccomp policy). This makes the smoke prove the profile loads,
# rather than running an unrelated hand-written approximation.
"$GATEWAY_BIN" -print-nsjail-profile smoke > "$SMOKE_DIR/profile.conf"
grep -q 'seccomp_string: "POLICY ' "$SMOKE_DIR/profile.conf"
grep -q 'clone_newpid: true;' "$SMOKE_DIR/profile.conf"
grep -q 'mount: { dst: "/proc"; fstype: "proc"' "$SMOKE_DIR/profile.conf"

echo "== envar sets the worker placement vars inside the jail =="
# Regression for the phantom keep_env syntax: nsjail's keep_env is a bool
# (pass EVERYTHING) and per-variable selection is repeated-string
# envar "K=V" entries. GW_WORKSPACE_DIR/GW_AGENT_HOME must be set via envar
# AND must actually reach the jailed process (the worker resolves its
# workspace/agent home from them and keys its nsjail sandbox capability off
# GW_AGENT_HOME).
grep -q 'envar: "GW_WORKSPACE_DIR=/workspace";' "$SMOKE_DIR/profile.conf"
grep -q 'envar: "GW_AGENT_HOME=/home/agent";' "$SMOKE_DIR/profile.conf"
"$NSJAIL_BIN" -Mo --config "$SMOKE_DIR/profile.conf" -- \
  /bin/sh -c 'test "$GW_WORKSPACE_DIR" = /workspace && test "$GW_AGENT_HOME" = /home/agent'

echo "== minimal jail with the real profile =="
"$NSJAIL_BIN" -Mo --config "$SMOKE_DIR/profile.conf" -- /bin/true

echo "== PID namespace: killing the wrapper reaps the whole tree =="
# Run the real profile with a command that forks a background sleeper. The
# wrapper (PID 1 inside the jail) must reap the whole tree when killed. Use
# `sleep` with a distinctive duration so we can detect the sleeper by comm name
# (the wrapper's own argv also mentions it, so we match `sleep` by name only).
"$NSJAIL_BIN" -Mo --config "$SMOKE_DIR/profile.conf" -- /bin/sh -c 'sleep 2891 & exec sleep 2891' &
WRAPPER_PID=$!

# Confirm the wrapper is alive and the sleeper actually spawned before killing.
sleep 2
if ! kill -0 "$WRAPPER_PID" 2>/dev/null; then
  echo "FAIL: nsjail wrapper exited before the tree-kill test" >&2
  exit 1
fi
if ! pgrep -x sleep >/dev/null 2>&1; then
  echo "FAIL: background sleeper never spawned in the jail" >&2
  exit 1
fi

kill -9 "$WRAPPER_PID" 2>/dev/null || true
wait "$WRAPPER_PID" 2>/dev/null || true

if pgrep -x sleep >/dev/null 2>&1; then
  echo "FAIL: a jailed process survived killing its nsjail wrapper" >&2
  exit 1
fi

echo "OK: nsjail smoke validation passed"
