from __future__ import annotations

from captain_hook import BaseHookEvent, Event, install_binary, on

from . import common

install_binary("../scripts/install-binary.sh")


@on(Event.SessionStart, async_=True)
def session_record(evt: BaseHookEvent) -> None:
    common.call_bin(evt, "session-record")
