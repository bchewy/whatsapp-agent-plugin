#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "$script_dir/bridge-env.sh"

cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/codex-whatsapp-plugin"
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/codex-whatsapp-plugin"
lock_dir="$cache_dir/bridge-start.lock"
pid_file="$state_dir/bridge.pid"
log_file="$state_dir/bridge.log"

# Health states: 0 = connected to WhatsApp, 2 = bridge process alive but not
# connected (it serves HTTP 503 with connection details), 1 = no response.
bridge_health_state() {
  python3 - "$WHATSAPP_BRIDGE_PORT" "$WHATSAPP_BRIDGE_TOKEN" <<'PY' >/dev/null 2>&1
import json
import sys
import urllib.error
import urllib.request

port, token = sys.argv[1], sys.argv[2]
request = urllib.request.Request(
    f"http://127.0.0.1:{port}/api/health",
    headers={"X-WhatsApp-Bridge-Token": token},
    method="GET",
)
try:
    with urllib.request.urlopen(request, timeout=0.5) as response:
        body = json.loads(response.read().decode("utf-8"))
except urllib.error.HTTPError as err:
    if err.code == 503:
        try:
            json.loads(err.read().decode("utf-8"))
        except Exception:
            raise SystemExit(1)
        raise SystemExit(2)
    raise SystemExit(1)
except Exception:
    raise SystemExit(1)
if body.get("success") is True:
    raise SystemExit(0)
raise SystemExit(2 if "connected" in body else 1)
PY
}

is_bridge_ready() {
  bridge_health_state
}

# True if any bridge process is running (port listener, built binary, or
# go run / go-build orphans), regardless of whether its API answers.
bridge_process_running() {
  bash "$script_dir/bridge-procs.sh" pids >/dev/null 2>&1
}

wait_for_bridge() {
  attempts="${1:-30}"
  while [ "$attempts" -gt 0 ]; do
    if is_bridge_ready; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 0.5
  done
  return 1
}

health_state=0
bridge_health_state || health_state=$?

if [ "$health_state" -eq 0 ]; then
  exit 0
fi

if [ "$health_state" -eq 2 ]; then
  # The bridge process is alive but disconnected from WhatsApp (e.g. client
  # outdated 405, stream replaced). Starting a second bridge on the same
  # session would make things worse; surface the state instead.
  printf 'warn: WhatsApp bridge on 127.0.0.1:%s is running but NOT connected to WhatsApp. Check GET /api/health last_error; if the client is outdated, rebuild with scripts/build-bridge.sh and restart the bridge.\n' "$WHATSAPP_BRIDGE_PORT" >&2
  exit 0
fi

if bridge_process_running; then
  # A bridge (or an orphaned go-run child) exists but its API is unresponsive.
  # It likely still holds the port, so a new bridge could not bind anyway.
  printf 'warn: a WhatsApp bridge process is already running but its API on 127.0.0.1:%s is unresponsive. Inspect with scripts/bridge-procs.sh list, stop it with scripts/bridge-procs.sh kill, then rerun.\n' "$WHATSAPP_BRIDGE_PORT" >&2
  exit 0
fi

umask 077
mkdir -p -m 700 "$WHATSAPP_MCP_STORE_DIR" "$cache_dir" "$state_dir"
chmod 700 "$WHATSAPP_MCP_STORE_DIR" "$cache_dir" "$state_dir"

if ! mkdir "$lock_dir" 2>/dev/null; then
  # Another MCP server instance is starting the bridge. Wait briefly and then
  # return control to the stdio MCP server; read-only tools can still use SQLite.
  wait_for_bridge 20 || true
  exit 0
fi
trap 'rmdir "$lock_dir" 2>/dev/null || true' EXIT

if is_bridge_ready; then
  exit 0
fi

{
  printf '\n[%s] Auto-starting WhatsApp bridge on 127.0.0.1:%s\n' "$(date -Iseconds)" "$WHATSAPP_BRIDGE_PORT"
  printf '[%s] Store: %s\n' "$(date -Iseconds)" "$WHATSAPP_MCP_STORE_DIR"
} >>"$log_file"

WHATSAPP_BRIDGE_SUPPRESS_QR=1 nohup bash "$script_dir/start-bridge.sh" >>"$log_file" 2>&1 </dev/null &
printf '%s\n' "$!" >"$pid_file"

if ! wait_for_bridge 30; then
  printf 'warn: WhatsApp bridge auto-started but did not pass the authenticated health check on 127.0.0.1:%s yet. See %s\n' "$WHATSAPP_BRIDGE_PORT" "$log_file" >&2
fi
