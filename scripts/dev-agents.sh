#!/usr/bin/env bash

# dev_agent_enabled prints a TOML boolean. In auto mode the first token of the
# configured command must resolve to an executable on PATH. Explicit on/off is
# useful for custom wrappers and negative-path testing.
dev_agent_enabled() {
  local mode="${1:-auto}"
  local command="${2:-}"
  local executable=""

  case "$mode" in
    on|ON|true|TRUE|yes|YES|1)
      printf 'true\n'
      return
      ;;
    off|OFF|false|FALSE|no|NO|0)
      printf 'false\n'
      return
      ;;
    auto|AUTO|'')
      ;;
    *)
      printf 'invalid agent enable mode %q (use auto, on, or off)\n' "$mode" >&2
      return 2
      ;;
  esac

  read -r executable _ <<<"$command"
  if [[ -n "$executable" ]] && command -v "$executable" >/dev/null 2>&1; then
    printf 'true\n'
  else
    printf 'false\n'
  fi
}
