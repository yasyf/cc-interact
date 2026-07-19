# /// script
# dependencies = ["capt-hook>=10.5.0"]
# ///

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

from captain_hook import Action, Event, app
from captain_hook.loader import discover_pack
from captain_hook.packs.manager import pack_module_name
from captain_hook.testing.helpers import mock_event

HOOKS = Path(__file__).resolve().parents[1]


class PackTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        app.reset()
        name = _pack_name()
        discover_pack(name, HOOKS)
        cls.package = f"captain_hook._packs.{pack_module_name(name)}"
        cls.common = sys.modules[f"{cls.package}.common"]
        cls.agent_plane = sys.modules[f"{cls.package}.agent_plane"]
        cls.guard_edit = sys.modules[f"{cls.package}.guard_edit"]
        cls.session = sys.modules[f"{cls.package}.session"]
        cls.turn_hooks = sys.modules[f"{cls.package}.turn_hooks"]
        cls.original_bin = cls.common.BIN

    @classmethod
    def tearDownClass(cls) -> None:
        cls.common.BIN = cls.original_bin
        app.reset()

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()

    def tearDown(self) -> None:
        self.common.BIN = self.original_bin
        self.tmp.cleanup()

    def stub(
        self,
        *,
        stdout: str = "",
        stdout_bytes: bytes | None = None,
        stderr: str = "",
        returncode: int = 0,
    ) -> Path:
        root = Path(self.tmp.name)
        record = root / "call.json"
        binary = root / "stub"
        output = stdout.encode() if stdout_bytes is None else stdout_bytes
        binary.write_text(
            "#!/usr/bin/env python3\n"
            "import json, pathlib, sys\n"
            f"pathlib.Path({str(record)!r}).write_text(json.dumps({{'argv': sys.argv[1:], 'stdin': sys.stdin.read()}}))\n"
            f"sys.stdout.buffer.write({output!r})\n"
            f"sys.stderr.write({stderr!r})\n"
            f"raise SystemExit({returncode})\n"
        )
        binary.chmod(0o755)
        self.common.BIN = binary
        return record

    def test_discover_pack_loads_every_module_and_hook(self) -> None:
        self.assertEqual(app._state.load_errors, [])
        self.assertEqual(len(app._state.hooks), 9)
        for module in ("agent_plane", "common", "guard_edit", "session", "turn_hooks"):
            self.assertIn(f"{self.package}.{module}", sys.modules)

        installer = next(
            hook.name for hook in app._state.hooks if hook.name.endswith(":install_binary_install_binary_sh")
        )
        expected = {
            "agent_start": (Event.SubagentStart, False, False),
            "agent_inject": (Event.PreToolUse, False, True),
            "agent_stop": (Event.SubagentStop, False, False),
            "agent_report": (Event.PostToolUse, True, True),
            "guard_edit": (Event.PreToolUse, False, True),
            installer: (Event.SessionStart, True, True),
            "session_record": (Event.SessionStart, True, True),
            "turn_start": (Event.UserPromptSubmit, False, True),
            "turn_end": (Event.Stop, False, True),
        }
        actual = {
            hook.name: (hook.spec.events, hook.spec.async_, hook.spec.skip_planning_agents)
            for hook in app._state.hooks
        }
        self.assertEqual(actual, expected)

    def test_guard_edit_blocks_only_exit_two(self) -> None:
        evt = mock_event(Event.PreToolUse, tool="Edit", file="x.go", content="new")
        self.stub(stderr="  review open: edits blocked  \n", returncode=2)
        result = self.guard_edit.guard_edit(evt)
        self.assertEqual(result.action, Action.block)
        self.assertEqual(result.message, "review open: edits blocked")

        self.stub(stderr="daemon unavailable\n", returncode=1)
        self.assertIsNone(self.guard_edit.guard_edit(evt))

    def test_agent_inject_emits_verbatim_context_without_approval(self) -> None:
        text = (
            "Directives from the demo steering channel (operator-authorized):\n"
            "- [operator #17] inspect the exact payload\n"
            "Act on each directive once, then continue your task."
        )
        envelope = json.dumps(
            {"hookSpecificOutput": {"hookEventName": "PreToolUse", "additionalContext": text}}
        )
        evt = mock_event(Event.PreToolUse, tool="Bash", command="go test ./...")
        self.stub(stdout=envelope + "\n")
        result = self.agent_plane.agent_inject(evt)
        self.assertEqual(result.action, Action.warn)
        self.assertEqual(result.message, text)
        self.assertFalse(result.approve)

    def test_agent_inject_rejects_invalid_envelopes(self) -> None:
        evt = mock_event(Event.PreToolUse, tool="Bash", command="go test ./...")
        for label, envelope in (
            (
                "wrong-event",
                {"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "text"}},
            ),
            ("missing-event", {"hookSpecificOutput": {"additionalContext": "text"}}),
            (
                "empty-context",
                {"hookSpecificOutput": {"hookEventName": "PreToolUse", "additionalContext": ""}},
            ),
        ):
            with self.subTest(case=label):
                self.stub(stdout=json.dumps(envelope))
                self.assertIsNone(self.agent_plane.agent_inject(evt))

    def test_agent_stop_translates_block_decision(self) -> None:
        evt = mock_event(Event.SubagentStop, agent_type="Explore", agent_id="a1")
        self.stub(stdout='{"decision":"block","reason":"finish the requested check"}\n')
        result = self.agent_plane.agent_stop(evt)
        self.assertEqual(result.action, Action.block)
        self.assertEqual(result.message, "finish the requested check")

    def test_parsing_hooks_fail_open(self) -> None:
        inject_evt = mock_event(Event.PreToolUse, tool="Bash", command="ls")
        stop_evt = mock_event(Event.SubagentStop, agent_type="worker", agent_id="a1")
        for label, stdout, returncode in (
            ("garbage", "not-json", 0),
            ("non-zero", "", 1),
            ("empty", "", 0),
        ):
            with self.subTest(hook="agent-inject", case=label):
                self.stub(stdout=stdout, returncode=returncode)
                self.assertIsNone(self.agent_plane.agent_inject(inject_evt))
            with self.subTest(hook="agent-stop", case=label):
                self.stub(stdout=stdout, returncode=returncode)
                self.assertIsNone(self.agent_plane.agent_stop(stop_evt))

    def test_side_effect_hooks_forward_raw_payload_and_return_none(self) -> None:
        cases = (
            (self.agent_plane.agent_start, "agent-start", mock_event(Event.SubagentStart, agent_id="a1")),
            (self.agent_plane.agent_report, "agent-report", mock_event(Event.PostToolUse, tool="Task", prompt="inspect")),
            (self.turn_hooks.turn_start, "turn-start", mock_event(Event.UserPromptSubmit, prompt="begin")),
            (self.turn_hooks.turn_end, "turn-end", mock_event(Event.Stop)),
            (self.session.session_record, "session-record", mock_event(Event.SessionStart, source="startup")),
        )
        for hook, subcommand, evt in cases:
            with self.subTest(subcommand=subcommand):
                evt._raw["contract_marker"] = subcommand
                record = self.stub(stdout="ignored output")
                self.assertIsNone(hook(evt))
                call = json.loads(record.read_text())
                self.assertEqual(call["argv"], [subcommand])
                self.assertEqual(json.loads(call["stdin"]), evt._raw)

    def test_invalid_utf8_output_fails_open_for_every_binary_hook(self) -> None:
        cases = (
            (self.agent_plane.agent_start, mock_event(Event.SubagentStart, agent_id="a1")),
            (self.agent_plane.agent_inject, mock_event(Event.PreToolUse, tool="Bash", command="ls")),
            (self.agent_plane.agent_stop, mock_event(Event.SubagentStop, agent_type="worker", agent_id="a1")),
            (self.agent_plane.agent_report, mock_event(Event.PostToolUse, tool="Task", prompt="inspect")),
            (self.guard_edit.guard_edit, mock_event(Event.PreToolUse, tool="Edit", file="x.go", content="new")),
            (self.turn_hooks.turn_start, mock_event(Event.UserPromptSubmit, prompt="begin")),
            (self.turn_hooks.turn_end, mock_event(Event.Stop)),
            (self.session.session_record, mock_event(Event.SessionStart, source="startup")),
        )
        for hook, evt in cases:
            with self.subTest(hook=hook.__name__):
                self.stub(stdout_bytes=b"\xff")
                self.assertIsNone(hook(evt))


def _pack_name() -> str:
    for line in (HOOKS / "capt-hook.toml").read_text().splitlines():
        if line.startswith("name = "):
            return line.split('"', 2)[1]
    raise AssertionError("pack manifest has no name")


if __name__ == "__main__":
    unittest.main()
