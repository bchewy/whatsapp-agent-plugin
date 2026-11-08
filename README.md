![WhatsApp Agent Plugin screenshot](whatsapp/assets/codex-whatsapp-plugin-screenshot.png)

# WhatsApp Agent Plugin

> Platform note: this plugin currently targets macOS only. The bridge runs as
> a macOS LaunchAgent, first-time pairing needs a QR scan from a terminal, and
> the helper scripts assume macOS tooling. Linux and Windows bridge support
> are intentionally out of scope for now. See
> [Runtime requirements and platform honesty](#runtime-requirements-and-platform-honesty).

Use your personal WhatsApp account from your coding agent, through an
unofficial third-party wrapper around
[`lharries/whatsapp-mcp`](https://github.com/lharries/whatsapp-mcp). The same
`whatsapp/` bundle works in three kinds of clients:

- **Codex** — installed as a plugin from a local marketplace
- **Grok Bot / Cursor (custom MCP)** — registered as a plain local stdio MCP
  server on a Mac that runs the bridge
- **Any Agent Plugins 1.0.0 client** — loaded from the portable `plugin.json`
  + `mcp.json`

This plugin lets your agent search contacts/chats/messages, download media,
and send WhatsApp messages or files through your personal WhatsApp
linked-device session.

> Naming note: the GitHub repository is now
> [`bchewy/whatsapp-agent-plugin`](https://github.com/bchewy/whatsapp-agent-plugin),
> matching the `telegram-agent-plugin` sibling. Old
> `codex-whatsapp-plugin` URLs redirect. Local cache/state/store paths keep
> the `codex-whatsapp-plugin` directory name so existing sessions are not
> broken.

This project is not affiliated with, endorsed by, or sponsored by WhatsApp,
Meta, or their affiliates. WhatsApp is a trademark of its respective owner, and
this project does not claim ownership of WhatsApp brand assets. The WhatsApp
logo is displayed only to identify the compatible service.

## Why MCP Instead Of A CLI?

[`wacli`](https://wacli.sh/) is a strong terminal-first option. It ships as a
single Go binary, pairs as a linked WhatsApp Web device, syncs messages into a
local SQLite/FTS5 store, and exposes search, send, media, contacts, chats,
groups, diagnostics, `--json`, `--events`, `--read-only`, and store-locking
workflows for scripts and humans. If your main workflow is shell scripting,
cron, or direct terminal use, a CLI may be the better fit.

This plugin chooses MCP because the primary user is an AI agent:

- The agent discovers named WhatsApp tools with schemas instead of learning
  command strings, flags, shell quoting, and output parsing rules.
- Contacts, chat JIDs, messages, media paths, and send parameters move through
  structured tool arguments and results.
- Write actions have an explicit tool boundary: the send tools require
  `confirm_send=true` after the exact recipient and content are confirmed.
- The plugin manifest, MCP entry, skill, health checks, setup scripts,
  and macOS background service install as one local integration.
- WhatsApp auth, indexed messages, downloaded media, and the bridge token stay
  in the local private store; data reaches the model only through tool results
  returned for the user's request.

In short: this is not MCP because CLIs are bad. It is MCP because WhatsApp in
an agent should feel like a typed local capability with explicit write gates,
not a shell subprocess the agent has to rediscover on every prompt.

## Runtime requirements and platform honesty

Every client path below depends on the same two processes:

1. `whatsapp-bridge`: a Go process that links to WhatsApp as a linked device,
   syncs messages into SQLite, and exposes `http://127.0.0.1:8080/api`. First
   pairing requires scanning a QR code from the WhatsApp mobile app, and the
   background-service install uses a macOS LaunchAgent.
2. `whatsapp-mcp-server`: a Python stdio MCP server that the client launches
   via `scripts/run-mcp.sh` and that talks to the bridge over localhost.

That means the MCP server is only useful on a machine that can also run the
paired bridge — today, a Mac. A Linux host (for example Grok Bot's default
cloud computer) cannot QR-pair WhatsApp or run the LaunchAgent-based bridge
service as-is. Until Linux bridge support exists, the honest support matrix
is:

- **Works today**: any client whose MCP stdio subprocess runs on a Mac that
  has already paired the bridge (Codex on that Mac, Cursor on that Mac, a
  Grok Bot / agent runtime executing on that Mac).
- **Packaging-ready, runtime out of scope**: Agent Plugins / Cursor installs
  on cloud Linux machines. The portable manifests load fine, but the server
  will not reach a bridge because there is no Linux bridge/pairing story yet.

## macOS Quick Start

This is the recommended path for a first-time macOS setup. It uses Codex as
the client; the bridge steps (1–3, 5) are identical for every other client.

1. Install the macOS build tools and dependencies:

   ```bash
   xcode-select --install
   brew install go python uv ffmpeg
   ```

   If you do not have Homebrew yet, install it from
   [brew.sh](https://brew.sh/) first. `ffmpeg` is optional, but recommended if
   you want WhatsApp voice-message/audio conversion to work smoothly.

2. From this repository root, check that the plugin can build and run:

   ```bash
   bash whatsapp/scripts/check-health.sh
   ```

3. Start the WhatsApp bridge in a visible terminal for the first pairing:

   ```bash
   bash whatsapp/scripts/start-bridge.sh
   ```

   On first run, scan the QR code from WhatsApp on your phone:
   `Settings > Linked Devices > Link a Device`. Leave this terminal open while
   you finish setup.

4. Install the local plugin into Codex from the shared local marketplace:

   ```bash
   codex plugin add whatsapp@local
   ```

   If the local marketplace does not include this checkout yet, add a
   `whatsapp` entry that points at this repo's `whatsapp/` directory, then rerun
   the command above. Keep the marketplace name as `local` and the display name
   as `Local Plugins` if you want it grouped with your other local plugins.

   Restart Codex and start a fresh thread after installing. Old threads can miss
   newly installed plugin, skill, and MCP context.

5. After the first QR pairing works, install the macOS background service so the
   bridge can keep running without a terminal window:

   ```bash
   bash whatsapp/scripts/bridge-service.sh install
   bash whatsapp/scripts/bridge-service.sh status
   ```

   For plugin upgrades, prefer running helper scripts from the installed plugin
   copy under `~/.codex/plugins/cache/...`; see
   [Install the plugin in Codex](#install-the-plugin-in-codex).

If setup fails, run `bash whatsapp/scripts/check-health.sh` again first. The
most common fixes are installing missing build tools, freeing port `8080`, or
starting `start-bridge.sh` in a visible terminal when a new QR code is needed.

### Agent Setup Prompt

Paste this into Codex from the repository root if you want an agent to run the
macOS setup for you:

```text
Set up this WhatsApp agent plugin on this Mac. Please install or verify the
needed macOS dependencies, run the health check, start the WhatsApp bridge for
first-time QR pairing if needed, install the local plugin into Codex, then set
up the macOS background service after pairing works. Do not send any WhatsApp
messages. Stop and ask me to scan the QR code if WhatsApp needs pairing, then
verify the bridge/service status and summarize what changed.
```

---

## Using with Codex

### Codex marketplace model

A Codex marketplace is a catalog of plugins. Its `interface.displayName` is the
dropdown label in Codex, while this plugin's `interface.displayName` is the
installable item shown inside that marketplace.

For a single local selector, keep all local plugin entries in one user-level
marketplace at `~/.agents/plugins/marketplace.json`. Do not keep a repo-local
`.agents/plugins/marketplace.json` active for this checkout unless you
intentionally want Codex to show this repository as a separate marketplace.

This repo's plugin bundle is `whatsapp/`. In the shared `Local Plugins`
marketplace, the plugin should be installed as:

```bash
codex plugin add whatsapp@local
```

The matching Telegram plugin
([`bchewy/telegram-agent-plugin`](https://github.com/bchewy/telegram-agent-plugin))
uses the same model: one `Local Plugins` marketplace, separate `whatsapp` and
`telegram` plugin entries.

For checkouts under `~/dev`, the relevant `plugins` entries look like this.
Preserve any other plugins already present in your local marketplace file.
The `whatsapp` path assumes a `whatsapp-agent-plugin` checkout directory; if
you cloned into a different directory name, adjust the `path` accordingly.

```json
{
  "name": "local",
  "interface": {
    "displayName": "Local Plugins"
  },
  "plugins": [
    {
      "name": "whatsapp",
      "source": {
        "source": "local",
        "path": "./dev/whatsapp-agent-plugin/whatsapp"
      },
      "policy": {
        "installation": "AVAILABLE",
        "authentication": "ON_INSTALL"
      },
      "category": "Productivity"
    },
    {
      "name": "telegram",
      "source": {
        "source": "local",
        "path": "./dev/telegram-agent-plugin/telegram"
      },
      "policy": {
        "installation": "AVAILABLE",
        "authentication": "ON_INSTALL"
      },
      "category": "Productivity"
    }
  ]
}
```

### Install the plugin in Codex

After the shared local marketplace includes this checkout:

```bash
codex plugin add whatsapp@local
```

If `whatsapp@local` is not found, add this checkout's `whatsapp/` directory to
`~/.agents/plugins/marketplace.json` under the existing `local` marketplace,
then rerun the install command. Do not keep a repo-local
`.agents/plugins/marketplace.json` active unless you intentionally want a
separate marketplace entry in the Codex dropdown.

Codex installs local plugins into `~/.codex/plugins/cache/...` and loads the
installed copy from there. After installing, prefer running helper scripts from
the installed copy so the bridge version matches the MCP server version:

```bash
PLUGIN_ROOT="$(python3 - <<'PY'
from pathlib import Path
roots = sorted(
    Path.home().glob(".codex/plugins/cache/*/whatsapp/*"),
    key=lambda p: p.stat().st_mtime,
    reverse=True,
)
print(roots[0] if roots else "")
PY
)"

bash "$PLUGIN_ROOT/scripts/check-health.sh"
bash "$PLUGIN_ROOT/scripts/start-bridge.sh"
```

Or install the background service from the installed plugin copy:

```bash
bash "$PLUGIN_ROOT/scripts/bridge-service.sh" install
```

If `PLUGIN_ROOT` is empty, install the plugin from the local marketplace first.
Runtime state still lives in the shared external store documented below, so
upgrades do not wipe your linked-device session.

---

## Using with Grok Bot / Cursor (custom MCP, stdio)

The bundled server is a normal local stdio MCP server, so any client that
supports custom MCP servers (Grok Bot, Cursor, Claude Desktop, etc.) can use
it directly — no Codex plugin machinery required.

**Read this first:** unlike the Telegram sibling plugin, this server cannot
run usefully on a generic Linux box. The MCP server only proxies a local
WhatsApp bridge, and the bridge (QR pairing, LaunchAgent service) is
macOS-only today. Two setups are supported:

1. **Client and server on the same Mac** (Cursor on your Mac, a Grok Bot /
   agent runtime executing on your Mac): works today, instructions below.
2. **Cloud Linux client machines** (for example Grok Bot's default computer):
   the packaging is ready — the portable manifests install fine — but the
   full runtime is out of scope until Linux bridge support exists. The Linux
   box cannot QR-pair WhatsApp or run the macOS bridge service, so MCP tool
   calls will fail to reach a bridge. Do not expect this path to work yet.

### 1. Clone the repo and pair the bridge (on the Mac)

```bash
git clone https://github.com/bchewy/whatsapp-agent-plugin.git
cd whatsapp-agent-plugin
bash whatsapp/scripts/check-health.sh
bash whatsapp/scripts/start-bridge.sh   # scan the QR code on first run
```

After the first pairing works, optionally install the background service so
the bridge survives without a terminal window:

```bash
bash whatsapp/scripts/bridge-service.sh install
```

### 2. Register the custom MCP server

Add a local **stdio** MCP server in your client with:

- **command**: `bash`
- **args**: `/absolute/path/to/whatsapp-agent-plugin/whatsapp/scripts/run-mcp.sh`
- **env** (optional):
  - `PYTHONUNBUFFERED=1` — recommended so server logs stream promptly
  - `WHATSAPP_BRIDGE_AUTO_START=0` — only if you manage the bridge yourself
    and do not want the MCP startup script to auto-start it
  - `WHATSAPP_BRIDGE_PORT` — only if the bridge runs on a non-default port

No WhatsApp credentials go in the MCP env. Auth lives in the local
linked-device store created during QR pairing.

In JSON-config clients (for example Cursor's `mcp.json`), the equivalent entry
looks like:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "bash",
      "args": [
        "/absolute/path/to/whatsapp-agent-plugin/whatsapp/scripts/run-mcp.sh"
      ],
      "env": {
        "PYTHONUNBUFFERED": "1"
      }
    }
  }
}
```

Use an absolute path to `run-mcp.sh`. Custom MCP servers are often spawned
with an unrelated working directory, so relative paths break. The script
locates the plugin bundle from its own path, so no `cwd` is required.

### 3. Verify

From the shell on the Mac:

```bash
bash whatsapp/scripts/check-health.sh
```

From the agent, in a fresh conversation, ask it to call `search_contacts` or
`list_chats`. You should see data from your own WhatsApp account. Start a
fresh thread after adding the MCP server; most clients only load new servers
into new sessions.

### Troubleshooting custom MCP setups

- **Tools error with connection failures**: the bridge is not running or not
  paired. Run `bash whatsapp/scripts/start-bridge.sh` in a visible terminal
  and scan the QR code if prompted.
- **Server registered but tools don't show up**: start a fresh thread /
  conversation after adding the MCP server.
- **First launch is slow**: `uv` resolves and builds the Python virtualenv on
  first run. If your client enforces a short startup timeout, run
  `bash whatsapp/scripts/check-health.sh` once beforehand — Codex's own config
  allows 60s for this reason.
- **Running the client on Linux**: not supported yet; see the platform
  honesty note above.

---

## Using with Agent Plugins clients

The installable package is the `whatsapp/` directory. It ships two manifest
sets side by side, so the same directory loads in Codex and in any client that
implements the portable [Agent Plugins 1.0.0](https://agent-plugins.org/specification)
format:

| File | Consumer | Purpose |
| --- | --- | --- |
| `plugin.json` | Agent Plugins clients | Portable manifest (`$schema` = `https://agent-plugins.org/schemas/1.0.0/plugin.schema.json`, closed field set) |
| `mcp.json` | Agent Plugins clients | Portable MCP config (`stdio` server launched via `bash`, `${PLUGIN_ROOT}`/`${PLUGIN_DATA}` placeholders) |
| `skills/<name>/SKILL.md` | Both | Agent Skills, discovered as immediate children of `skills/` by both formats |
| `.codex-plugin/plugin.json` | Codex | Codex-native manifest, including marketplace `interface` metadata (display name, logo, screenshots, prompts) |
| `.mcp.json` | Codex | Codex-native MCP config, including Codex-only fields (`startup_timeout_sec`, `tool_timeout_sec`) |

Notes on the split:

- Codex does not currently document an Agent Plugins `extensions` namespace,
  so its UI/marketplace metadata stays in `.codex-plugin/plugin.json` instead
  of being duplicated under an invented `extensions` key. If Codex publishes a
  reverse-domain namespace later, that metadata can move into the portable
  manifest's `extensions` field.
- The portable `mcp.json` declares no secret values. WhatsApp auth comes from
  the one-time QR pairing and lives in the local private store, not in the
  package or the MCP environment.
- The portable config sets `UV_PROJECT_ENVIRONMENT=${PLUGIN_DATA}/uv-env` so
  `uv` builds the Python virtualenv in the client-managed writable data
  directory instead of inside the (possibly read-only) installed package.
  `run-mcp.sh` respects an existing `UV_PROJECT_ENVIRONMENT` and only falls
  back to its own cache location when unset (the Codex path).
- Shared metadata (`name`, `version`, `author`, `homepage`, `repository`,
  `license`, `keywords`) must stay identical across `plugin.json` and
  `.codex-plugin/plugin.json`, and the two MCP configs must launch the same
  server command. The test suite enforces this
  (`tests/whatsapp_plugin/test_plugin_structure.py`), and also validates the
  portable files against the vendored Agent Plugins 1.0.0 schemas.
- Runtime constraint applies here too: an Agent Plugins client can load this
  package anywhere, but the MCP server only produces useful results on a Mac
  with a paired bridge. See the platform honesty section above.

An Agent Plugins client loads the package by reading root `plugin.json`,
discovering skills under `skills/`, and starting `whatsapp` from root
`mcp.json`. Codex keeps using `.codex-plugin/plugin.json` and `.mcp.json`
exactly as before; nothing about the Codex install flow changed.

---

## What It Bundles

The actual plugin bundle is `whatsapp/`, mirroring the layout used by
[`telegram-agent-plugin`](https://github.com/bchewy/telegram-agent-plugin).
Local marketplace registration lives outside this repo in
`~/.agents/plugins/marketplace.json`.

- `whatsapp/plugin.json`: portable Agent Plugins 1.0.0 manifest.
- `whatsapp/mcp.json`: portable Agent Plugins 1.0.0 MCP server declaration.
- `whatsapp/.codex-plugin/plugin.json`: Codex plugin manifest.
- `whatsapp/.mcp.json`: Codex MCP server entry for WhatsApp.
- `whatsapp/assets/icon.svg`: WhatsApp logo used in the composer and plugin UI.
- `whatsapp/skills/whatsapp/SKILL.md`: workflow guidance for the agent.
- `whatsapp/scripts/`: setup, health, bridge, watchdog, and reset helpers.
- `whatsapp/build/vendor/whatsapp-mcp/`: vendored upstream WhatsApp bridge and
  MCP server.
- `tests/whatsapp_plugin/`: structure tests that validate the portable
  manifests against the vendored Agent Plugins schemas and keep the Codex and
  portable files in sync.

The upstream integration is two processes:

1. `whatsapp-bridge`: Go process that links to WhatsApp, syncs messages into SQLite, and exposes `http://127.0.0.1:8080/api`.
2. `whatsapp-mcp-server`: Python stdio MCP server that the client starts via `scripts/run-mcp.sh`.

Python dependencies are installed by `uv` into
`${XDG_CACHE_HOME:-$HOME/.cache}/codex-whatsapp-plugin/` so the plugin tree
stays clean (Agent Plugins clients override this to `${PLUGIN_DATA}/uv-env`).

## Prerequisites

The macOS quick-start command above installs the normal dependency set. In
detail, the plugin needs:

- macOS (see the platform note at the top)
- Go 1.25+ or a Go toolchain with automatic toolchain downloads enabled
- Python 3.11+
- `uv`
- WhatsApp mobile app/account for QR pairing
- C compiler/CGO support for `go-sqlite3`
- Optional: `ffmpeg` for converting non-Opus audio into WhatsApp voice messages

If you are not using Homebrew, install `uv` directly:

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

## Local Development

Check setup:

```bash
bash whatsapp/scripts/check-health.sh
```

Start the WhatsApp bridge:

```bash
bash whatsapp/scripts/start-bridge.sh
```

On first run, scan the QR code from WhatsApp:
`Settings > Linked Devices > Link a Device`.

Keep the bridge terminal open while the agent uses the MCP tools, or install
the macOS background service below.

### Run The Bridge In The Background

After the first QR pairing succeeds, you can run the bridge as a macOS
LaunchAgent so you do not need to keep a terminal window open:

```bash
bash whatsapp/scripts/bridge-service.sh install
```

Useful service commands:

```bash
bash whatsapp/scripts/bridge-service.sh status
bash whatsapp/scripts/bridge-service.sh logs
bash whatsapp/scripts/bridge-service.sh restart
bash whatsapp/scripts/bridge-service.sh uninstall
```

The MCP startup script also tries to auto-start the bridge in the background
when the client starts the WhatsApp MCP server. Set
`WHATSAPP_BRIDGE_AUTO_START=0` to disable that behavior.

For an always-on box, `whatsapp/scripts/watchdog.sh` can run periodically
(cron) to restart the bridge when it is actually wedged. It restarts when the
process or `/api/health` is down, and when `messages.db` has not changed for
20+ minutes (`WHATSAPP_WATCHDOG_SIGNAL_STALE_SEC`, default 1200) **and** the
post-restart bridge log contains a socket-death signal — matched
case-insensitively, including whatsmeow's live wording `Keepalive timed out`
and `failed to acquire lock: context deadline exceeded`, plus
`StreamReplaced` and `Client outdated (405)`. Staleness alone never restarts
(a quiet chat is not an outage), and a grace window
(`WHATSAPP_WATCHDOG_GRACE_SEC`, default 600) protects a freshly restarted
bridge. All kills go through `bridge-procs.sh`, so an unrelated process is
never killed and a second bridge is never started against the same session.

Logs are written under:

```text
${XDG_STATE_HOME:-$HOME/.local/state}/codex-whatsapp-plugin/
```

If your linked-device session expires and a new QR code is needed, use
`start-bridge.sh` in a visible terminal or inspect `bridge-service.sh logs`.

### Run the structure tests

The repo ships structural conformance tests for the plugin bundle: the
portable `plugin.json`/`mcp.json` are validated against the vendored
Agent Plugins 1.0.0 schemas, skills discovery is checked, and the Codex and
portable manifests are kept in sync. Run them from the repository root:

```bash
uv run --no-project --with pytest --with jsonschema pytest tests
```

## Tools

The bundled upstream MCP server exposes:

- `search_contacts`
- `list_messages`
- `list_chats`
- `list_events`
- `backfill_events`
- `list_desktop_events`
- `get_chat`
- `get_direct_chat_by_contact`
- `get_contact_chats`
- `get_last_interaction`
- `get_message_context`
- `send_message`
- `send_file`
- `send_audio_message`
- `create_group`
- `add_group_participants`
- `add_or_invite_group_participants`
- `get_group_invite_link`
- `download_media`

Use read-only tools first to confirm contacts, chat JIDs, and message context.
`list_events` returns event cards captured by the bridge. If older event cards
are missing, use `backfill_events` to request on-demand history sync for the
chat, then run `list_events` again. On macOS, if an event appears in WhatsApp
Desktop's group info event drawer but is still missing from the bridge index,
open that drawer and use `list_desktop_events` to read the visible Desktop
event rows via accessibility.
For sends, confirm the final recipient and content before calling send tools.
For group participant changes, prefer invite links for raw phone numbers. Direct
adds by raw phone number may be rejected by WhatsApp with participant-level 403
errors, and that path has been observed to log out linked devices.
The send tools also require `confirm_send=true` as an explicit final step.
For group creation, confirm the exact group name and participant list; the tool
requires `confirm_create=true`. For group participant adds, confirm the exact
group JID and participant list; the tool requires `confirm_add=true`. For
fallback invite DMs after failed adds, also confirm the fallback message and set
`confirm_invite_message=true`.

## Reset Auth Or Message State

If WhatsApp gets out of sync or linked-device auth expires:

```bash
bash whatsapp/scripts/reset-session.sh
```

The script requires typing `RESET` and removes only local WhatsApp SQLite
session/index files. You will need to scan the QR code again.

## Private Data

Runtime data lives outside the plugin install directory so it survives plugin
upgrades:

```text
${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/store/
```

That directory contains WhatsApp auth state, indexed messages, and downloaded
media, plus a local bridge token used to authenticate MCP-to-bridge HTTP
requests. The bridge creates it with `0700` permissions; do not commit or share
its contents.

If you previously paired against the legacy in-tree location
(`whatsapp/vendor/whatsapp-mcp/whatsapp-bridge/store/`), move its contents into
the new path once before restarting the bridge:

```bash
NEW_STORE="${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/store"
mkdir -p "$NEW_STORE"
mv whatsapp/vendor/whatsapp-mcp/whatsapp-bridge/store/* "$NEW_STORE/" 2>/dev/null || true
```

The vendored bridge is patched to bind only to `127.0.0.1`, redact message
contents from terminal logs by default, sanitize downloaded media filenames,
and store downloaded media with owner-only file permissions. Set
`WHATSAPP_MCP_DEBUG_CONTENT=1` only when you explicitly need content-level
bridge logs.

### Optional Environment Overrides

Both the bridge and the MCP server respect:

- `WHATSAPP_MCP_STORE_DIR`: absolute path for SQLite stores and downloaded
  media. Defaults to `${XDG_DATA_HOME:-$HOME/.local/share}/codex-whatsapp-plugin/store`.
- `WHATSAPP_BRIDGE_PORT`: TCP port for the local bridge REST API. Defaults to
  `8080`. Override if `8080` is already in use on your machine.
- `WHATSAPP_MCP_DEBUG_CONTENT=1`: re-enable content-level bridge logs (off by
  default).
- `WHATSAPP_BRIDGE_TOKEN`: shared token for local MCP-to-bridge HTTP requests.
  The helper scripts create and store one automatically under the private store
  directory.

## License

This repository's wrapper code is MIT licensed. The vendored upstream project
is MIT licensed by Luke Harries; see `NOTICE.md` and
`whatsapp/build/vendor/whatsapp-mcp/LICENSE`.

Runtime dependencies keep their own licenses. This source release includes Go
and Python dependency manifests; if you distribute built bridge binaries, review
and comply with dependency licenses, including `go.mau.fi/whatsmeow` and
`go.mau.fi/libsignal`.
