"""Unit tests for the bridge watchdog in scripts/watchdog.sh.

The script's `scan-log` mode feeds bridge-log lines through the same
socket-death matcher the live run uses, and `decide` drives the pure
restart-decision function with synthetic inputs, so the detection and the
restart matrix are testable without real processes, sockets, or databases.

Regression context (Sun 8 Nov 2026 SGT): whatsmeow logged "Keepalive timed
out" while the watchdog grepped for "KeepAlive timeout", so repeated keepalive
bursts left ingest silently stuck for ~35 minutes with /api/health still
returning 200 connected=true; a watchdog run printed "ok stale=2123s
health=200" and did nothing.

Run from the repository root with:

    python3 -m pytest tests -q
"""

from __future__ import annotations

import subprocess
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "whatsapp" / "scripts" / "watchdog.sh"

BASE_ENV = {"PATH": "/usr/bin:/bin"}

# Verbatim lines from the live bridge.log (whatsmeow WARN output, including
# the ANSI color codes the logger emits).
LIVE_KEEPALIVE_LINE = "10:48:38.721\x1b[33m [Client WARN] Keepalive timed out\x1b[0m"
LIVE_SOCKET_LOCK_LINE = (
    "10:51:28.988\x1b[33m [Client/Socket WARN] Error sending close to websocket: "
    "failed to close WebSocket: failed to acquire lock: context deadline exceeded\x1b[0m"
)
# The spelling the pre-fix box watchdog matched (older whatsmeow wording).
OLD_KEEPALIVE_LINE = "12:00:00.000 [Client ERROR] KeepAlive timeout"

INNOCUOUS_LINES = [
    "10:00:00.000 [Client INFO] Successfully authenticated",
    "10:00:01.000 [Database INFO] Upgrading database to v12",
    "10:00:02.000 Received a message from +0000000000",
]

RESTART_MARKER = "2026-11-08T10:52:00+08:00 started pid=12345 from /home/user/.local/share/codex-whatsapp-plugin/bin/whatsapp-bridge"


def scan(lines: list[str]) -> bool:
    result = subprocess.run(
        ["bash", str(SCRIPT), "scan-log"],
        input="".join(line + "\n" for line in lines),
        capture_output=True,
        text=True,
        env=BASE_ENV,
    )
    assert result.returncode in (0, 1), result.stderr
    return result.returncode == 0


def decide(**inputs: object) -> tuple[str, str]:
    """Runs `watchdog.sh decide` and returns (action, detail).

    Keyword args are uppercased into WD_* / WHATSAPP_WATCHDOG_* environment
    variables, e.g. stale=2123 -> WD_STALE=2123, grace_sec=600 ->
    WHATSAPP_WATCHDOG_GRACE_SEC=600.
    """
    env = dict(BASE_ENV)
    wd_keys = {
        "health_code",
        "health_ok",
        "alive",
        "stale",
        "since_restart",
        "has_marker",
        "log_suspect",
        "outdated",
        "outdated_recent",
    }
    for key, value in inputs.items():
        if key in wd_keys:
            env[f"WD_{key.upper()}"] = str(value)
        else:
            env[f"WHATSAPP_WATCHDOG_{key.upper()}"] = str(value)
    result = subprocess.run(
        ["bash", str(SCRIPT), "decide"],
        capture_output=True,
        text=True,
        env=env,
    )
    assert result.returncode == 0, result.stderr
    action, _, detail = result.stdout.strip().partition("\t")
    return action, detail


class TestLogSignalMatcher:
    def test_live_keepalive_wording_matches(self) -> None:
        assert scan(INNOCUOUS_LINES + [LIVE_KEEPALIVE_LINE])

    def test_live_websocket_lock_deadline_matches(self) -> None:
        assert scan(INNOCUOUS_LINES + [LIVE_SOCKET_LOCK_LINE])

    def test_old_keepalive_timeout_wording_still_matches(self) -> None:
        assert scan([OLD_KEEPALIVE_LINE])

    def test_matching_is_case_insensitive(self) -> None:
        assert scan(["10:00:00.000 [Client WARN] KEEPALIVE TIMED OUT"])
        assert scan(["10:00:00.000 [Client WARN] keepalive timed out"])

    def test_stream_replaced_matches(self) -> None:
        assert scan(["10:00:00.000 [Client ERROR] Got StreamReplaced, disconnecting"])

    def test_client_outdated_matches(self) -> None:
        assert scan(["10:00:00.000 [Client ERROR] Client outdated (405) connect failure"])

    def test_innocuous_lines_do_not_match(self) -> None:
        assert not scan(INNOCUOUS_LINES)

    def test_empty_log_does_not_match(self) -> None:
        assert not scan([])

    def test_signal_before_restart_marker_is_ignored(self) -> None:
        # Signals from the previous bridge process must not restart the new one.
        assert not scan([LIVE_KEEPALIVE_LINE, LIVE_SOCKET_LOCK_LINE, RESTART_MARKER])

    def test_signal_after_restart_marker_matches(self) -> None:
        assert scan(
            [LIVE_SOCKET_LOCK_LINE, RESTART_MARKER] + INNOCUOUS_LINES + [LIVE_KEEPALIVE_LINE]
        )

    def test_watchdog_restart_line_itself_is_not_a_signal(self) -> None:
        # The RESTART reason mentions keepalive; the marker reset must keep the
        # watchdog's own log lines from re-triggering it.
        assert not scan(
            [
                "2026-11-08T10:52:00+08:00 RESTART: messages.db stale 2123s + "
                "keepalive/socket-death log signal while health ok",
                RESTART_MARKER,
            ]
        )


class TestRestartDecision:
    def test_incident_repro_health_ok_stale_with_signal_restarts(self) -> None:
        # The Sun 8 Nov 2026 incident: health 200 connected=true, messages.db
        # stale 2123s, keepalive signals in the log. The live watchdog printed
        # "ok stale=2123s health=200"; it must restart instead.
        action, detail = decide(
            health_code=200,
            health_ok=1,
            alive=1,
            stale=2123,
            since_restart=68468,
            has_marker=1,
            log_suspect=1,
        )
        assert action == "restart"
        assert "2123" in detail

    def test_stale_without_signal_does_not_restart(self) -> None:
        # Quiet chats must not flap the bridge just because nobody texted.
        action, detail = decide(
            health_code=200,
            health_ok=1,
            alive=1,
            stale=7200,
            since_restart=90000,
            has_marker=1,
            log_suspect=0,
        )
        assert action == "ok"
        assert "not restarting" in detail

    def test_signal_within_grace_window_does_not_restart(self) -> None:
        action, detail = decide(
            health_code=200,
            health_ok=1,
            alive=1,
            stale=1300,
            since_restart=120,
            has_marker=1,
            log_suspect=1,
            grace_sec=600,
        )
        assert action == "ok"
        assert "grace" in detail

    def test_signal_but_fresh_messages_does_not_restart(self) -> None:
        # A keepalive blip that recovered on its own: ingest is current.
        action, _ = decide(
            health_code=200,
            health_ok=1,
            alive=1,
            stale=300,
            since_restart=90000,
            has_marker=1,
            log_suspect=1,
        )
        assert action == "ok"

    def test_default_signal_stale_threshold_is_twenty_minutes(self) -> None:
        common = dict(
            health_code=200,
            health_ok=1,
            alive=1,
            since_restart=90000,
            has_marker=1,
            log_suspect=1,
        )
        assert decide(stale=1200, **common)[0] == "ok"
        assert decide(stale=1201, **common)[0] == "restart"

    def test_dead_process_restarts_regardless_of_log(self) -> None:
        action, _ = decide(
            health_code="000",
            health_ok=0,
            alive=0,
            stale=10,
            since_restart=90000,
            has_marker=1,
            log_suspect=0,
        )
        assert action == "restart"

    def test_health_200_without_connected_restarts(self) -> None:
        action, _ = decide(
            health_code=200,
            health_ok=0,
            alive=1,
            stale=10,
            since_restart=90000,
            has_marker=1,
            log_suspect=0,
        )
        assert action == "restart"

    def test_health_503_fresh_disconnect_waits(self) -> None:
        # The bridge reports its own reconnect attempts; give it time.
        action, detail = decide(
            health_code=503,
            health_ok=0,
            alive=1,
            stale=300,
            since_restart=90000,
            has_marker=1,
            log_suspect=1,
        )
        assert action == "ok"
        assert "not restarting yet" in detail

    def test_health_503_stale_with_signal_restarts(self) -> None:
        action, _ = decide(
            health_code=503,
            health_ok=0,
            alive=1,
            stale=1300,
            since_restart=90000,
            has_marker=1,
            log_suspect=1,
        )
        assert action == "restart"

    def test_health_503_stale_with_signal_within_grace_waits(self) -> None:
        action, _ = decide(
            health_code=503,
            health_ok=0,
            alive=1,
            stale=1300,
            since_restart=120,
            has_marker=1,
            log_suspect=1,
            grace_sec=600,
        )
        assert action == "ok"

    def test_client_outdated_restarts_with_rate_limit_counter(self) -> None:
        action, detail = decide(
            health_code=503,
            health_ok=0,
            alive=1,
            stale=10,
            since_restart=90000,
            has_marker=1,
            outdated=1,
            outdated_recent=0,
        )
        assert action == "restart"
        assert "outdated" in detail

    def test_client_outdated_rate_limit_stops_restart_loop(self) -> None:
        action, detail = decide(
            health_code=503,
            health_ok=0,
            alive=1,
            stale=10,
            since_restart=90000,
            has_marker=1,
            outdated=1,
            outdated_recent=3,
            outdated_max=3,
        )
        assert action == "ok"
        assert "rebuild" in detail
