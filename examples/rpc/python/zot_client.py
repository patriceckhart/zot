"""Minimal Python client for the `zot rpc` JSON protocol.

Usage:
    python zot_client.py "fix the failing test"

Spawns `zot rpc`, sends one prompt, prints assistant text as it streams,
and exits when the turn finishes. No external dependencies — stdlib
only. Implements just enough of the protocol to be useful as a
starting point; see docs/rpc.md for the full schema.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import uuid


class ZotClient:
    def __init__(self, *flags: str) -> None:
        argv = ["zot", "rpc", *flags]
        env = os.environ.copy()
        self.proc = subprocess.Popen(
            argv,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=sys.stderr,
            env=env,
            text=True,
            bufsize=1,  # line-buffered
        )
        self._lock = threading.Lock()

    def send(self, **command) -> str:
        """Send one command, return its `id`."""
        if "id" not in command:
            command["id"] = uuid.uuid4().hex[:8]
        line = json.dumps(command)
        with self._lock:
            assert self.proc.stdin is not None
            self.proc.stdin.write(line + "\n")
            self.proc.stdin.flush()
        return command["id"]

    def events(self):
        """Yield every JSON object the server emits, until stdout closes."""
        assert self.proc.stdout is not None
        for line in self.proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                yield json.loads(line)
            except json.JSONDecodeError:
                continue

    def close(self) -> None:
        if self.proc.stdin is not None:
            try:
                self.proc.stdin.close()
            except Exception:
                pass
        self.proc.wait(timeout=5)


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: zot_client.py <prompt>", file=sys.stderr)
        return 2
    prompt = " ".join(sys.argv[1:])

    client = ZotClient()
    try:
        token = os.environ.get("ZOTCORE_RPC_TOKEN")
        if token:
            client.send(type="hello", token=token)

        client.send(type="prompt", message=prompt)

        for ev in client.events():
            t = ev.get("type")
            if t == "text_delta":
                sys.stdout.write(ev.get("delta", ""))
                sys.stdout.flush()
            elif t == "tool_call":
                print(
                    f"\n[tool] {ev.get('name')}({json.dumps(ev.get('args', {}))})",
                    file=sys.stderr,
                )
            elif t == "tool_result":
                if ev.get("is_error"):
                    print("[tool error]", file=sys.stderr)
            elif t == "usage":
                cum = ev.get("cumulative", {})
                print(
                    f"\n[usage] cum input={cum.get('input')} "
                    f"output={cum.get('output')} cost=${cum.get('cost_usd', 0):.4f}",
                    file=sys.stderr,
                )
            elif t == "done":
                break
            elif t == "error":
                print(f"\n[error] {ev.get('message')}", file=sys.stderr)
                return 1
        print()
        return 0
    finally:
        client.close()


if __name__ == "__main__":
    sys.exit(main())
