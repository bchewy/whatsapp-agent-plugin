#!/usr/bin/env bash
set -euo pipefail

# Detects running WhatsApp bridge processes without relying on a pidfile, so
# watchdogs and ensure-bridge.sh see every bridge instance:
#   * processes listening on the bridge port (WHATSAPP_BRIDGE_PORT)
#   * processes whose command is the built bridge binary (WHATSAPP_BRIDGE_BIN)
#   * processes whose command references a .../whatsapp-mcp/whatsapp-bridge
#     vendor path (e.g. `go build -o ... whatsapp-bridge` or an explicit path)
#   * `go run main.go` parents and their orphaned go-build children
#     (.../go-build*/.../exe/main) whose working directory is a vendored
#     whatsapp-mcp/whatsapp-bridge directory
#
# Usage:
#   bridge-procs.sh [list]      Print "PID<TAB>COMMAND" for each bridge process.
#   bridge-procs.sh pids        Print bridge PIDs only, one per line.
#   bridge-procs.sh kill        SIGTERM bridge processes matched by command/cwd
#                               (never an unrecognized port squatter), escalate
#                               to SIGKILL after 5s if still alive.
#   bridge-procs.sh match-stdin Read "PID<TAB>CWD<TAB>COMMAND" lines from stdin
#                               and print matching PIDs. Used by unit tests.
#
# Exit status for list/pids: 0 if at least one bridge process was found, 1 if none.

port="${WHATSAPP_BRIDGE_PORT:-8080}"
bridge_bin="${WHATSAPP_BRIDGE_BIN:-${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/bin/whatsapp-bridge}"

# Matches a process by command line and working directory. $1=cwd $2=command.
# Matching is deliberately conservative (executable path, not substrings of
# arguments) so e.g. an editor opened on the bridge source never matches.
is_bridge_command() {
  local cwd="$1" cmd="$2"
  local exe="${cmd%% *}"

  # The built bridge binary.
  if [ "$exe" = "$bridge_bin" ]; then
    return 0
  fi

  # A binary executed from inside a vendored bridge directory.
  case "$exe" in
    */whatsapp-mcp/whatsapp-bridge/*) return 0 ;;
  esac

  # `go run main.go` parents and orphaned go-build children are only
  # identifiable through their working directory inside the vendored bridge.
  case "$cwd" in
    */whatsapp-mcp/whatsapp-bridge)
      case "$cmd" in
        *"go run main.go"*) return 0 ;;
      esac
      case "$exe" in
        */go-build*/exe/*) return 0 ;;
      esac
      ;;
  esac

  return 1
}

# Prints the working directory of a PID, or an empty line if unavailable.
proc_cwd() {
  local pid="$1"
  if [ -e "/proc/$pid/cwd" ]; then
    readlink "/proc/$pid/cwd" 2>/dev/null || true
  elif command -v lsof >/dev/null 2>&1; then
    lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' | head -n 1
  fi
}

# Prints PIDs listening on the bridge port.
port_listener_pids() {
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true
  fi
}

match_stdin() {
  local found=1
  while IFS=$'\t' read -r pid cwd cmd; do
    [ -n "${pid:-}" ] || continue
    if is_bridge_command "${cwd:-}" "${cmd:-}"; then
      printf '%s\n' "$pid"
      found=0
    fi
  done
  return "$found"
}

# Prints "PID<TAB>COMMAND" for every process matched by command/cwd.
matched_processes() {
  local pid cmd cwd
  while read -r pid cmd; do
    [ -n "$pid" ] || continue
    [ "$pid" != "$$" ] || continue
    cwd=""
    # Only resolve cwd for plausible candidates to keep this cheap.
    case "$cmd" in
      *whatsapp*|*go\ run\ main.go*|*/go-build*/exe/*) cwd="$(proc_cwd "$pid")" ;;
    esac
    if is_bridge_command "$cwd" "$cmd"; then
      printf '%s\t%s\n' "$pid" "$cmd"
    fi
  done < <(ps -eo pid=,command= 2>/dev/null || ps axo pid=,command=)
}

# Prints "PID<TAB>COMMAND" for port listeners not already matched above.
port_processes() {
  local pid cmd
  for pid in $(port_listener_pids); do
    cmd="$(ps -p "$pid" -o command= 2>/dev/null || true)"
    printf '%s\t%s\n' "$pid" "${cmd:-<unknown command, listening on port $port>}"
  done
}

list_processes() {
  { matched_processes; port_processes; } | sort -u -t "$(printf '\t')" -k1,1n
}

kill_bridge() {
  local pids pid
  pids="$(matched_processes | cut -f1)"
  if [ -z "$pids" ]; then
    echo "No bridge processes matched by command/cwd; nothing to kill." >&2
    # Surface port squatters so the operator can decide what to do with them.
    for pid in $(port_listener_pids); do
      echo "note: PID $pid is listening on port $port but was not recognized as the bridge; not killing it." >&2
    done
    return 0
  fi

  for pid in $pids; do
    echo "Sending SIGTERM to bridge process $pid" >&2
    kill "$pid" 2>/dev/null || true
  done

  local deadline=$((SECONDS + 5))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local alive=0
    for pid in $pids; do
      if kill -0 "$pid" 2>/dev/null; then
        alive=1
      fi
    done
    [ "$alive" -eq 1 ] || return 0
    sleep 0.2
  done

  for pid in $pids; do
    if kill -0 "$pid" 2>/dev/null; then
      echo "Bridge process $pid survived SIGTERM; sending SIGKILL" >&2
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
}

cmd="${1:-list}"
case "$cmd" in
  list)
    output="$(list_processes)"
    [ -n "$output" ] && printf '%s\n' "$output"
    [ -n "$output" ]
    ;;
  pids)
    output="$(list_processes | cut -f1)"
    [ -n "$output" ] && printf '%s\n' "$output"
    [ -n "$output" ]
    ;;
  kill)
    kill_bridge
    ;;
  match-stdin)
    match_stdin
    ;;
  *)
    echo "Usage: bridge-procs.sh [list|pids|kill|match-stdin]" >&2
    exit 2
    ;;
esac
