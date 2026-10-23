"""Unit tests for the bridge process detection in scripts/bridge-procs.sh.

The script's `match-stdin` mode feeds synthetic "PID<TAB>CWD<TAB>COMMAND"
process-table lines through the same matching function the live `list`,
`pids`, and `kill` modes use, so the kill-safety rules are testable without
real processes.

Run from the repository root with:

    uv run --no-project --with pytest --with jsonschema pytest tests
"""

from __future__ import annotations

import subprocess
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "whatsapp" / "scripts" / "bridge-procs.sh"

BRIDGE_BIN = "/home/user/.local/share/codex-whatsapp-plugin/bin/whatsapp-bridge"
VENDOR_CWD = "/home/user/.codex/plugins/whatsapp/build/vendor/whatsapp-mcp/whatsapp-bridge"


def match(lines: list[tuple[str, str, str]]) -> list[str]:
    stdin = "".join(f"{pid}\t{cwd}\t{cmd}\n" for pid, cwd, cmd in lines)
    result = subprocess.run(
        ["bash", str(SCRIPT), "match-stdin"],
        input=stdin,
        capture_output=True,
        text=True,
        env={"PATH": "/usr/bin:/bin", "WHATSAPP_BRIDGE_BIN": BRIDGE_BIN},
    )
    assert result.returncode in (0, 1), result.stderr
    return result.stdout.split()


class TestBridgeProcessDetection:
    def test_built_binary_matches(self) -> None:
        assert match([("100", "/", BRIDGE_BIN)]) == ["100"]

    def test_binary_inside_vendor_dir_matches(self) -> None:
        assert match([("101", "/", f"{VENDOR_CWD}/whatsapp-bridge")]) == ["101"]

    def test_go_run_parent_in_vendor_cwd_matches(self) -> None:
        assert match([("102", VENDOR_CWD, "go run main.go")]) == ["102"]

    def test_orphaned_go_build_child_in_vendor_cwd_matches(self) -> None:
        assert match([("103", VENDOR_CWD, "/tmp/go-build123456/b001/exe/main")]) == ["103"]

    def test_go_build_child_outside_vendor_cwd_does_not_match(self) -> None:
        assert match([("104", "/home/user/other-project", "/tmp/go-build123456/b001/exe/main")]) == []

    def test_go_run_outside_vendor_cwd_does_not_match(self) -> None:
        assert match([("105", "/home/user/other-project", "go run main.go")]) == []

    def test_whatsapp_desktop_app_does_not_match(self) -> None:
        assert match([("106", "/", "/Applications/WhatsApp.app/Contents/MacOS/WhatsApp")]) == []

    def test_editor_on_bridge_source_does_not_match(self) -> None:
        assert match([("107", "/home/user", f"vim {VENDOR_CWD}/main.go")]) == []

    def test_grep_mentioning_binary_path_does_not_match(self) -> None:
        assert match([("108", "/home/user", f"grep whatsapp {BRIDGE_BIN}")]) == []

    def test_mixed_process_table(self) -> None:
        pids = match(
            [
                ("1", "/", "/sbin/init"),
                ("200", VENDOR_CWD, "go run main.go"),
                ("201", VENDOR_CWD, "/tmp/go-build99/b001/exe/main"),
                ("202", "/", BRIDGE_BIN),
                ("203", "/home/user", "python3 -m http.server 8080"),
                ("204", "/", "/usr/bin/whatsapp-web-helper"),
            ]
        )
        assert pids == ["200", "201", "202"]

    def test_no_match_exits_nonzero(self) -> None:
        result = subprocess.run(
            ["bash", str(SCRIPT), "match-stdin"],
            input="300\t/\t/sbin/init\n",
            capture_output=True,
            text=True,
            env={"PATH": "/usr/bin:/bin", "WHATSAPP_BRIDGE_BIN": BRIDGE_BIN},
        )
        assert result.returncode == 1
        assert result.stdout == ""
