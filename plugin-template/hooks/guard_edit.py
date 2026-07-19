from __future__ import annotations

import json
import subprocess

from captain_hook import Allow, BaseHookEvent, Event, HookResult, Input, Tool, on

from . import common


@on(
    Event.PreToolUse,
    only_if=[Tool("Edit", "Write", "NotebookEdit")],
    tests={Input(command="ls"): Allow()},
)
def guard_edit(evt: BaseHookEvent) -> HookResult | None:
    if not common.BIN.exists():
        return None
    try:
        evt.ctx.call_cli(
            [str(common.BIN), "guard-edit"],
            input=json.dumps(evt._raw),
            timeout=10,
            throw=True,
        )
    except subprocess.CalledProcessError as exc:
        if exc.returncode == 2:
            return evt.block(exc.stderr.strip())
    except (OSError, subprocess.SubprocessError, UnicodeDecodeError):
        pass
    return None
