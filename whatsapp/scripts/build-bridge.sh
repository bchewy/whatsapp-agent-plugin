#!/usr/bin/env bash
set -euo pipefail

# Builds the vendored Go bridge to a stable binary path so runtime scripts can
# exec a real binary instead of `go run` (which leaves orphaned go-build
# children that watchdogs cannot attribute to the bridge).
#
# Output path: $WHATSAPP_BRIDGE_BIN, defaulting to
#   ${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/bin/whatsapp-bridge

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
plugin_root="$(cd "$script_dir/.." && pwd)"
bridge_dir="$plugin_root/build/vendor/whatsapp-mcp/whatsapp-bridge"

bin_path="${WHATSAPP_BRIDGE_BIN:-${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/bin/whatsapp-bridge}"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to build the WhatsApp bridge. Install Go, then rerun this script." >&2
  exit 127
fi

if [ ! -f "$bridge_dir/main.go" ]; then
  echo "Missing vendored WhatsApp bridge at $bridge_dir." >&2
  exit 1
fi

umask 077
mkdir -p "$(dirname "$bin_path")"

# Build to a temp file and rename so a running bridge binary is never
# overwritten in place (and a failed build never clobbers a working binary).
tmp_bin="$bin_path.build.$$"
trap 'rm -f "$tmp_bin"' EXIT

echo "Building WhatsApp bridge from $bridge_dir ..." >&2
(cd "$bridge_dir" && go build -o "$tmp_bin" .)
mv -f "$tmp_bin" "$bin_path"
trap - EXIT

echo "Built WhatsApp bridge binary at $bin_path" >&2
