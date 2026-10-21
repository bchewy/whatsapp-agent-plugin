#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
plugin_root="$(cd "$script_dir/.." && pwd)"
bridge_dir="$plugin_root/build/vendor/whatsapp-mcp/whatsapp-bridge"

# Stable bridge state directory, port, binary path, and local auth token
# shared with the Python MCP server.
source "$script_dir/bridge-env.sh"

# Prefer the prebuilt binary; build it if missing. Never `go run`: it leaves
# orphaned go-build children that watchdogs cannot attribute to the bridge.
if [ ! -x "$WHATSAPP_BRIDGE_BIN" ]; then
  bash "$script_dir/build-bridge.sh"
fi

if [ ! -x "$WHATSAPP_BRIDGE_BIN" ]; then
  echo "WhatsApp bridge binary missing at $WHATSAPP_BRIDGE_BIN after build; aborting." >&2
  exit 1
fi

cat >&2 <<EOF
Starting the WhatsApp bridge on http://127.0.0.1:${WHATSAPP_BRIDGE_PORT}/api.
State directory: ${WHATSAPP_MCP_STORE_DIR}

On first run, scan the QR code shown in this terminal from WhatsApp:
Settings > Linked Devices > Link a Device.

Keep this process running while Codex uses the WhatsApp MCP tools.
EOF

cd "$bridge_dir"
exec "$WHATSAPP_BRIDGE_BIN"
