#!/bin/sh
#
# nsjail smoke validation for Linux CI. nsjail is Linux-only; this runs in the
# built image on Linux CI, NOT on the darwin dev host.
#
# Validates:
#   1. `nsjail --version` — the binary runs.
#   2. `ldd` — every shared-library dependency resolves.
#   3. A minimal smoke jail (`mode: ONCE` + private mount namespace) executes
#      a trivial command inside the jail.
#
# Usage: docker/nsjail-smoke.sh   (must run inside the runtime image, or on a
# Linux host with nsjail on PATH).
set -eu

NSJAIL_BIN="${NSJAIL_BIN:-/usr/local/bin/nsjail}"

echo "== nsjail --version =="
"$NSJAIL_BIN" --version

echo "== ldd =="
ldd "$NSJAIL_BIN"

echo "== minimal smoke jail =="
# nsjail -Mo: mode ONCE, one jail, exit when the command exits.
# A private mount namespace + user namespace (unprivileged, no CAP_SYS_ADMIN)
# is the phase-1 boundary; /bin/true must run inside it.
"$NSJAIL_BIN" -Mo --config /dev/stdin -- /bin/true <<'EOF'
mode: ONCE;
clone_newns: true;
clone_newpid: false;
clone_newipc: true;
clone_newuts: true;
clone_newnet: false;
uidmap: { inside_id: "65532"; outside_id: "65532"; count: "1"; };
gidmap: { inside_id: "65532"; outside_id: "65532"; count: "1"; };
user: "65532";
group: "65532";
tmpfs: { dst: "/tmp"; rw: true; };
EOF

echo "OK: nsjail smoke validation passed"
