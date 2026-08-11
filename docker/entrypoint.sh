#!/bin/sh
#
# agent-gateway container entrypoint.
#
# The gateway binary is the API process AND the worker Supervisor; the
# supervisor forks one nsjail-wrapped gateway-worker per session. The
# entrypoint just prepares the shared runtime dirs, writes a default config
# when none is mounted, and then exec's Gateway beneath Tini. Signal
# propagation API -> worker(nsjail) -> CLI and session-aware process-group
# teardown happen inside the supervisor; Tini is the container-level reaper.
#
# Runtime dirs (both are created idempotently, so they also work on a fresh
# persistent volume):
#   GATEWAY_WORKSPACE_ROOT  controlled workspace root, default /srv/workspaces
#   GATEWAY_RUNTIME_DIR     per-session sockets/agent-homes/profiles,
#                           default /gateway-run
#   GATEWAY_CONFIG          TOML config path, default /etc/gateway/gateway.toml
#   GATEWAY_TOKEN           optional bearer token (written into the config)
set -eu

WORKSPACE_ROOT="${GATEWAY_WORKSPACE_ROOT:-/srv/workspaces}"
RUNTIME_DIR="${GATEWAY_RUNTIME_DIR:-/gateway-run}"
CONFIG_PATH="${GATEWAY_CONFIG:-/etc/gateway/gateway.toml}"

mkdir -p "$WORKSPACE_ROOT" "$RUNTIME_DIR"

# Write a default config when the deployment did not mount one. The default
# requires nsjail (fail-closed) at /usr/local/bin/nsjail and pins the same
# version the image was built from; deployments override by mounting their own
# config at GATEWAY_CONFIG.
if [ ! -f "$CONFIG_PATH" ]; then
  mkdir -p "$(dirname "$CONFIG_PATH")"
  {
    printf 'mode = "prod"\n\n'
    printf '[server]\nlisten_addr = ":4096"\ndrain_timeout = "30s"\n\n'
    printf '[worker]\nstop_grace_period = "10s"\n'
    printf 'heartbeat_interval = "15s"\nheartbeat_timeout = "3s"\nheartbeat_failures = 3\n\n'
    printf '[sessions]\nidle_timeout = "2h"\nreap_interval = "1m"\n\n'
    printf '[auth]\nrequired = true\n'
    if [ -n "${GATEWAY_TOKEN:-}" ]; then
      escaped_token=$(printf '%s' "$GATEWAY_TOKEN" | sed 's/\\/\\\\/g; s/"/\\"/g')
      printf 'callers = [{ id = "container", tokens = ["%s"] }]\n' "$escaped_token"
    else
      printf 'callers = [] # set GATEWAY_TOKEN or mount a production config\n'
    fi
    printf '\n[workspace]\nroot = "%s"\n\n' "$WORKSPACE_ROOT"
    printf '[isolation]\nrequired = true\nnsjail_version = "3.6"\n'
    printf 'nsjail_source = "https://github.com/google/nsjail"\n'
    printf 'binary_path = "/usr/local/bin/nsjail"\n\n'
    printf '[isolation.mounts]\nworkspace_dir = "/workspace"\n'
    printf 'agent_home_dir = "/agent-home"\ntmp_dir = "/tmp"\n\n'
    printf '[isolation.user_namespace]\nenabled = true\nuid = 65532\ngid = 65532\n\n'
    printf '[isolation.seccomp]\npolicy = "kafel"\n'
  } > "$CONFIG_PATH"
  echo "[entrypoint] wrote default gateway config to $CONFIG_PATH"
fi

# nsjail must exist and be executable before we hand over to the gateway: the
# gateway's readiness probe reports 503 when preflight fails, but a missing
# binary at this stage indicates a broken image, so fail fast here.
if [ ! -x /usr/local/bin/nsjail ]; then
  echo "[entrypoint] FATAL: /usr/local/bin/nsjail missing or not executable" >&2
  exit 1
fi

# exec replaces the shell beneath Tini. The configured CMD ("gateway") and
# any extra args are forwarded.
exec "$@"
