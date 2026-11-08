#!/usr/bin/env bash
set -euo pipefail

# Watchdog for a long-running WhatsApp bridge (cron/launchd periodic run).
#
# This is the versioned source of the box watchdog that previously lived only
# at ~/.local/share/codex-whatsapp-plugin/bin/watchdog.sh. It restarts the
# bridge when it is actually dead or wedged, and deliberately does NOT restart
# on quiet periods alone (nobody texting is not an outage).
#
# Restart triggers, in order of precedence:
#   1. /api/health 503 with a "client outdated (405)" last_error — restart so
#      the operator notices, rate-limited to OUTDATED_MAX restarts per
#      OUTDATED_WINDOW_SEC (a restart cannot fix a 405; rebuilding can).
#   2. No bridge process / no authorized health response / health 200 without
#      success:true+connected:true — plain process death.
#   3. messages.db untouched for more than SIGNAL_STALE_SEC (default 20 min)
#      AND the post-restart bridge log contains a socket-death signal
#      (keepalive timed out, websocket lock deadline, StreamReplaced, ...).
#      This fires even while health says connected=true: whatsmeow keepalive
#      failures can leave ingest silently stuck while the event-driven health
#      tracker still reports the last known connected state (observed Sun
#      8 Nov 2026 SGT: repeated "Keepalive timed out" bursts, messages.db
#      stale for ~35 min, health 200 the whole time).
#   Staleness alone, with no log signal, never restarts.
#
# A restart grace window (RESTART_GRACE_SEC) keeps a freshly restarted bridge
# from being immediately killed again while it catches up.
#
# All process kills go through bridge-procs.sh, which only terminates
# processes positively identified as the bridge by command path/cwd — never an
# unrelated PID-reuse victim or an unrecognized port squatter. This script
# refuses to restart if bridge-procs.sh is missing.
#
# Test-only modes (no file or process access):
#   watchdog.sh scan-log   Read log lines on stdin; exit 0 if a socket-death
#                          signal appears after the most recent restart marker.
#   watchdog.sh decide     Read WD_* inputs from the environment and print the
#                          decision as "restart<TAB>reason" or "ok<TAB>message".

# Restart when messages.db is stale this long AND a socket-death log signal is
# present (trigger 3 above). 20 minutes: long enough that an ordinary
# keepalive blip recovers on its own first, short enough that a wedged socket
# does not stay wedged for an hour.
SIGNAL_STALE_SEC="${WHATSAPP_WATCHDOG_SIGNAL_STALE_SEC:-1200}"
# Staleness beyond this is mentioned in the "ok" output (visibility only; a
# quiet chat with no death signal never causes a restart).
MAX_STALE_SEC="${WHATSAPP_WATCHDOG_STALE_SEC:-3600}"
RESTART_GRACE_SEC="${WHATSAPP_WATCHDOG_GRACE_SEC:-600}"
LOG_TAIL_LINES="${WHATSAPP_WATCHDOG_LOG_TAIL_LINES:-400}"
OUTDATED_MAX="${WHATSAPP_WATCHDOG_OUTDATED_MAX:-3}"
OUTDATED_WINDOW_SEC="${WHATSAPP_WATCHDOG_OUTDATED_WINDOW_SEC:-1800}"

# Socket-death signals, matched case-insensitively against the bridge log.
# Live whatsmeow writes "[Client WARN] Keepalive timed out" and
# "[Client/Socket WARN] Error sending close to websocket: failed to close
# WebSocket: failed to acquire lock: context deadline exceeded"; older
# builds/notes used "KeepAlive timeout". Match all spellings so a whatsmeow
# wording change cannot silently disable detection again.
SUSPECT_PATTERN='StreamReplaced|exiting so the bridge|keepalive timed out|keepalive timeout|Client/Socket ERROR|client outdated \(405\)|failed to acquire lock: context deadline exceeded'

# Reads log lines on stdin, drops everything up to and including the most
# recent restart marker (the watchdog's own "RESTART:"/"started pid=" lines),
# and exits 0 if a socket-death signal remains in what the current bridge
# process has logged since.
scan_log_stdin() {
  awk '
    /RESTART:|started pid=/ { buf=""; next }
    { buf = buf $0 ORS }
    END { printf "%s", buf }
  ' | grep -q -i -E "$SUSPECT_PATTERN"
}

# Pure decision function; prints "restart<TAB>reason" or "ok<TAB>message".
# $1 health_code  HTTP status from /api/health ("000" for no response)
# $2 health_ok    1 if 200 with success:true and connected:true (or absent)
# $3 alive        1 if any bridge process exists
# $4 stale        seconds since messages.db (or the restart marker) changed
# $5 since_restart  seconds since this watchdog last restarted the bridge
# $6 has_marker   1 if a restart marker exists (since_restart is meaningful)
# $7 log_suspect  1 if scan_log_stdin found a post-restart socket-death signal
# $8 outdated     1 if health is 503 with a client-outdated (405) last_error
# $9 outdated_recent  outdated-restart count within OUTDATED_WINDOW_SEC
decide() {
  local health_code="$1" health_ok="$2" alive="$3" stale="$4" \
    since_restart="$5" has_marker="$6" log_suspect="$7" \
    outdated="$8" outdated_recent="$9"

  local within_grace=0
  if [[ "$has_marker" -eq 1 && "$since_restart" -lt "$RESTART_GRACE_SEC" ]]; then
    within_grace=1
  fi

  if [[ "$outdated" -eq 1 ]]; then
    if [[ "$outdated_recent" -ge "$OUTDATED_MAX" ]]; then
      printf 'ok\tWARN: 503 client outdated (405) but %s restarts in last %ss; not restarting (rebuild with scripts/build-bridge.sh)\n' \
        "$outdated_recent" "$OUTDATED_WINDOW_SEC"
    else
      printf 'restart\thealth 503 client outdated (405) (restart %s/%s in window)\n' \
        "$((outdated_recent + 1))" "$OUTDATED_MAX"
    fi
    return 0
  fi

  if [[ "$health_code" == "503" ]]; then
    # Alive but disconnected (reconnecting, stream replaced): give whatsmeow a
    # chance to recover; restart only when ingest is stale AND the log shows a
    # socket-death signal, outside the post-restart grace window.
    if [[ "$stale" -gt "$SIGNAL_STALE_SEC" && "$log_suspect" -eq 1 && "$within_grace" -eq 0 ]]; then
      printf 'restart\thealth 503 + messages.db stale %ss + socket-death log signal\n' "$stale"
    else
      printf 'ok\thealth 503 (disconnected) stale=%ss; not restarting yet\n' "$stale"
    fi
    return 0
  fi

  if [[ "$alive" -eq 0 || "$health_ok" -eq 0 ]]; then
    printf 'restart\tprocess/health down (alive=%s health_code=%s health_ok=%s)\n' \
      "$alive" "$health_code" "$health_ok"
    return 0
  fi

  if [[ "$stale" -gt "$SIGNAL_STALE_SEC" && "$log_suspect" -eq 1 ]]; then
    if [[ "$within_grace" -eq 1 ]]; then
      printf 'ok\tstale=%ss suspect_log but within grace health=%s\n' "$stale" "$health_code"
    else
      printf 'restart\tmessages.db stale %ss + keepalive/socket-death log signal while health ok\n' "$stale"
    fi
    return 0
  fi

  if [[ "$stale" -gt "$MAX_STALE_SEC" ]]; then
    printf 'ok\tstale=%ss health=%s (quiet; no socket-death log signal - not restarting)\n' \
      "$stale" "$health_code"
    return 0
  fi

  printf 'ok\tstale=%ss health=%s\n' "$stale" "$health_code"
}

mode="${1:-run}"
case "$mode" in
  scan-log)
    if scan_log_stdin; then exit 0; fi
    exit 1
    ;;
  decide)
    decide \
      "${WD_HEALTH_CODE:-200}" \
      "${WD_HEALTH_OK:-1}" \
      "${WD_ALIVE:-1}" \
      "${WD_STALE:-0}" \
      "${WD_SINCE_RESTART:-0}" \
      "${WD_HAS_MARKER:-0}" \
      "${WD_LOG_SUSPECT:-0}" \
      "${WD_OUTDATED:-0}" \
      "${WD_OUTDATED_RECENT:-0}"
    exit 0
    ;;
  run) ;;
  *)
    echo "Usage: watchdog.sh [run|scan-log|decide]" >&2
    exit 2
    ;;
esac

# Everything below needs the real box environment; the test-only modes above
# must stay independent of it.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

STORE="${WHATSAPP_MCP_STORE_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/store}"
STATE="${WHATSAPP_BRIDGE_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/codex-whatsapp-plugin}"
PERSIST_BIN="${WHATSAPP_BRIDGE_PERSIST_BIN:-${WHATSAPP_BRIDGE_BIN:-${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/bin/whatsapp-bridge}}"
TOKEN_FILE="$STORE/bridge.token"
LOG="$STATE/bridge.log"
PID_FILE="$STATE/bridge.pid"
MARKER="$STATE/last_restart"
PORT="${WHATSAPP_BRIDGE_PORT:-8080}"
PROCS_SH="${WHATSAPP_BRIDGE_PROCS_SH:-$script_dir/bridge-procs.sh}"
OUTDATED_LOG="$STATE/outdated_restarts"

mkdir -p -m 700 "$STATE"

[[ -f "$TOKEN_FILE" ]] || { echo "fail: missing $TOKEN_FILE" >&2; exit 1; }
[[ -x "$PERSIST_BIN" ]] || { echo "fail: missing $PERSIST_BIN" >&2; exit 1; }
# Restarts must go through bridge-procs.sh so we never kill an unrelated
# process or leave an old bridge fighting a new one over the same session.
[[ -f "$PROCS_SH" ]] || {
  echo "fail: missing bridge-procs.sh at $PROCS_SH (set WHATSAPP_BRIDGE_PROCS_SH)" >&2
  exit 1
}

token=$(tr -d '\r\n' < "$TOKEN_FILE")
now=$(date +%s)
db_mtime=$(stat -c %Y "$STORE/messages.db" 2>/dev/null || stat -f %m "$STORE/messages.db" 2>/dev/null || echo 0)
marker_mtime=$(stat -c %Y "$MARKER" 2>/dev/null || stat -f %m "$MARKER" 2>/dev/null || echo 0)
activity=$db_mtime
[[ "$marker_mtime" -gt "$activity" ]] && activity=$marker_mtime
stale=$((now - activity))
since_restart=$((now - marker_mtime))
has_marker=0
[[ "$marker_mtime" -gt 0 ]] && has_marker=1

alive=0
if WHATSAPP_BRIDGE_BIN="$PERSIST_BIN" WHATSAPP_BRIDGE_PORT="$PORT" \
    bash "$PROCS_SH" pids >/dev/null 2>&1; then
  alive=1
fi
if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then alive=1; fi

health_body="$(mktemp "${TMPDIR:-/tmp}/wa-health.XXXXXX")"
trap 'rm -f "$health_body"' EXIT
health_code=$(curl -sS -m 2 -o "$health_body" -w '%{http_code}' \
  -H "X-WhatsApp-Bridge-Token: $token" \
  "http://127.0.0.1:${PORT}/api/health" 2>/dev/null || echo 000)

# Any authorized JSON response (200 or 503) means the bridge process is alive.
if [[ "$health_code" == "200" || "$health_code" == "503" ]]; then alive=1; fi

outdated=0
if [[ "$health_code" == "503" ]]; then
  if python3 - "$health_body" <<'PY' 2>/dev/null
import json, sys
body = json.load(open(sys.argv[1]))
err = str(body.get("last_error") or "").lower()
sys.exit(0 if ("405" in err or "outdated" in err) else 1)
PY
  then
    outdated=1
  fi
fi

health_ok=0
if [[ "$health_code" == "200" ]]; then
  if python3 - "$health_body" <<'PY' 2>/dev/null
import json, sys
body = json.load(open(sys.argv[1]))
ok = body.get("success") and (("connected" not in body) or body.get("connected") is True)
sys.exit(0 if ok else 1)
PY
  then
    health_ok=1
  fi
fi

log_suspect=0
if [[ -f "$LOG" ]]; then
  # Before the first watchdog restart there is no marker line in the log, so
  # only trust a short tail; afterwards scan a longer window — the marker
  # lines inside it reset the scan to post-restart output only.
  tail_lines="$LOG_TAIL_LINES"
  [[ "$has_marker" -eq 1 ]] || tail_lines=80
  if tail -n "$tail_lines" "$LOG" | scan_log_stdin; then
    log_suspect=1
  fi
fi

outdated_recent=0
if [[ "$outdated" -eq 1 && -f "$OUTDATED_LOG" ]]; then
  outdated_recent=$(awk -v cut=$((now - OUTDATED_WINDOW_SEC)) '$1 >= cut' "$OUTDATED_LOG" | wc -l | tr -d ' ')
fi

decision=$(decide "$health_code" "$health_ok" "$alive" "$stale" \
  "$since_restart" "$has_marker" "$log_suspect" "$outdated" "$outdated_recent")
action="${decision%%$'\t'*}"
detail="${decision#*$'\t'}"

if [[ "$action" != "restart" ]]; then
  echo "$(date -Iseconds) $detail"
  exit 0
fi

if [[ "$outdated" -eq 1 ]]; then
  echo "$now" >> "$OUTDATED_LOG"
fi

echo "$(date -Iseconds) RESTART: $detail" | tee -a "$LOG"

# Kill every identifiable bridge process (built binary, vendored-dir binary,
# go-run orphans, and anything started via the legacy state-dir symlink).
WHATSAPP_BRIDGE_BIN="$PERSIST_BIN" WHATSAPP_BRIDGE_PORT="$PORT" \
  bash "$PROCS_SH" kill 2>/dev/null || true
if [[ "$STATE/whatsapp-bridge" != "$PERSIST_BIN" ]]; then
  WHATSAPP_BRIDGE_BIN="$STATE/whatsapp-bridge" WHATSAPP_BRIDGE_PORT="$PORT" \
    bash "$PROCS_SH" kill 2>/dev/null || true
fi
sleep 1

ln -sfn "$PERSIST_BIN" "$STATE/whatsapp-bridge"
export WHATSAPP_MCP_STORE_DIR="$STORE"
export WHATSAPP_BRIDGE_PORT="$PORT"
export WHATSAPP_BRIDGE_TOKEN="$token"
export WHATSAPP_BRIDGE_SUPPRESS_QR=1
export GODEBUG="${GODEBUG:-netdns=go}"
nohup "$PERSIST_BIN" >>"$LOG" 2>&1 </dev/null &
echo $! > "$PID_FILE"
date -Iseconds > "$MARKER"
echo "$(date -Iseconds) started pid=$(cat "$PID_FILE") from $PERSIST_BIN" | tee -a "$LOG"
