#!/usr/bin/env bash
set -euo pipefail

# Shared runtime environment for the Go bridge and Python MCP server.
export WHATSAPP_MCP_STORE_DIR="${WHATSAPP_MCP_STORE_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/store}"
export WHATSAPP_BRIDGE_PORT="${WHATSAPP_BRIDGE_PORT:-8080}"
export WHATSAPP_BRIDGE_BIN="${WHATSAPP_BRIDGE_BIN:-${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/bin/whatsapp-bridge}"

umask 077
mkdir -p -m 700 "$WHATSAPP_MCP_STORE_DIR"
chmod 700 "$WHATSAPP_MCP_STORE_DIR"

if [ -z "${WHATSAPP_BRIDGE_TOKEN:-}" ]; then
  token_file="$WHATSAPP_MCP_STORE_DIR/bridge.token"
  python3 - "$token_file" <<'PY'
import os
import secrets
import sys

path = sys.argv[1]
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    os.chmod(path, 0o600)
else:
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        handle.write(secrets.token_urlsafe(32) + "\n")
PY
  export WHATSAPP_BRIDGE_TOKEN="$(tr -d '\r\n' < "$token_file")"
fi

if [ -z "$WHATSAPP_BRIDGE_TOKEN" ]; then
  echo "WHATSAPP_BRIDGE_TOKEN is empty; refusing to start WhatsApp bridge tooling." >&2
  exit 1
fi
