from __future__ import annotations

import json
from pathlib import Path

from captain_hook import BaseHookEvent

__capt_hook_skip__ = True

PLUGIN_ROOT = Path(__file__).resolve().parents[1]
BIN = PLUGIN_ROOT / "bin" / "{{BINARY_NAME}}"


def call_bin(evt: BaseHookEvent, sub: str, *, timeout: int = 10) -> str | None:
    if not BIN.exists():
        return None
    try:
        return evt.ctx.call_cli(
            [str(BIN), sub],
            input=json.dumps(evt._raw),
            timeout=timeout,
            throw=False,
        )
    except UnicodeDecodeError:
        return None
