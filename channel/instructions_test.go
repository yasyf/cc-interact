package channel

import "testing"

func TestInstructions(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec InstructionsSpec
		want string
	}{
		{
			name: "cc-orchestrate",
			spec: InstructionsSpec{
				Desc:          "the cc-orchestrate channel",
				Traffic:       "Orchestrator directives arrive",
				Source:        "plugin:cc-orchestrate:cc-orchestrate",
				Guide:         `An orchestrate.message event is a directive: its "text" field is an instruction from the orchestrator to act on. Other event types, such as status frames, are informational and need no reply.`,
				SilentOutside: "an orchestrated child session",
			},
			want: `This MCP server is the cc-orchestrate channel. Orchestrator directives arrive as <channel source="plugin:cc-orchestrate:cc-orchestrate" type="..."> tags whose inner JSON has a "type" field identifying the event.

An orchestrate.message event is a directive: its "text" field is an instruction from the orchestrator to act on. Other event types, such as status frames, are informational and need no reply.

The channel never speaks unsolicited: outside an orchestrated child session it is silent, and silence needs nothing from you.`,
		},
		{
			name: "cc-review",
			spec: InstructionsSpec{
				Desc:    "the cc-review code-review channel",
				Traffic: "Review activity reaches you",
				Source:  "cc-review",
				Guide: `A channel.probe tag may arrive right after this session runs cc-review start — it is a delivery check, not a request: run "${CLAUDE_PLUGIN_ROOT}/bin/cc-review" channel-ack --session "$CLAUDE_CODE_SESSION_ID" --cwd "$PWD", and reply nothing.

Real review input arrives as other event types such as comment.created, comment.updated, ai.request.created, and submit. The cc-review:start skill governs how to handle those.`,
				SilentOutside: "a /cc-review:start run",
			},
			want: `This MCP server is the cc-review code-review channel. Review activity reaches you as <channel source="cc-review" type="..."> tags whose inner JSON has a "type" field identifying the event.

A channel.probe tag may arrive right after this session runs cc-review start — it is a delivery check, not a request: run "${CLAUDE_PLUGIN_ROOT}/bin/cc-review" channel-ack --session "$CLAUDE_CODE_SESSION_ID" --cwd "$PWD", and reply nothing.

Real review input arrives as other event types such as comment.created, comment.updated, ai.request.created, and submit. The cc-review:start skill governs how to handle those.

The channel never speaks unsolicited: outside a /cc-review:start run it is silent, and silence needs nothing from you.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Instructions(tc.spec); got != tc.want {
				t.Fatalf("Instructions() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReceiveProtocol(t *testing.T) {
	spec := ReceiveProtocolSpec{
		Watch:      `'/usr/local/bin/cco' watch --session 123e4567 --cwd '/tmp/work dir'`,
		Source:     "plugin:cc-orchestrate:cc-orchestrate",
		Ack:        `'/usr/local/bin/cco' channel-ack --session 123e4567 --cwd '/tmp/work dir'`,
		DedupeHint: `Deduplicate by the message's "id" field: the same message may arrive on both paths around the switchover.`,
	}
	want := `1. Immediately, before doing anything else, arm a persistent Monitor running exactly this command:

    '/usr/local/bin/cco' watch --session 123e4567 --cwd '/tmp/work dir'

2. Messages may also arrive as <channel source="plugin:cc-orchestrate:cc-orchestrate"> tags. On the FIRST such tag, run this command exactly once:

    '/usr/local/bin/cco' channel-ack --session 123e4567 --cwd '/tmp/work dir'

Then stop the watch Monitor with TaskStop and rely on channel tags from then on.

3. Delivery is at-least-once, and the watch and channel have independent cursors. Deduplicate by the message's "id" field: the same message may arrive on both paths around the switchover.`
	if got := ReceiveProtocol(spec); got != want {
		t.Fatalf("ReceiveProtocol() = %q, want %q", got, want)
	}
}

func TestRelayStep(t *testing.T) {
	want := `4. When a <channel source="cc-interact"> tag carries an agent.relay event naming an agent_id, SendMessage that agent a wake ONLY: tell it that it has pending directives — they arrive with its next tool call or when it calls the await tool — and identify yourself as the sender. Do NOT include any directive content: the mailbox is the single delivery channel, and a repeated agent.relay tag for the same agent is safe to re-nudge.`
	if got := RelayStep("cc-interact"); got != want {
		t.Fatalf("RelayStep() = %q, want %q", got, want)
	}
}
