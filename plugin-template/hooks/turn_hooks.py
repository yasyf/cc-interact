from __future__ import annotations

from captain_hook import BaseHookEvent, Event, on

from . import common

# Delete this file if the plugin has no per-turn state.


@on(Event.UserPromptSubmit)
def turn_start(evt: BaseHookEvent) -> None:
    common.call_bin(evt, "turn-start")


@on(Event.Stop)
def turn_end(evt: BaseHookEvent) -> None:
    common.call_bin(evt, "turn-end")
