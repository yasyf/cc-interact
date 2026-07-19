package channel

// ReceiveProtocolSpec parameterizes ReceiveProtocol, the tri-state receive
// protocol a consumer hands an agent that must not miss channel traffic.
type ReceiveProtocolSpec struct {
	// Watch is the exact watch invocation the agent runs under its Monitor,
	// pre-quoted by the consumer.
	Watch string
	// Source is the source attribute the consumer's channel renders in its tags.
	Source string
	// Ack is the exact channel-ack invocation, pre-quoted by the consumer.
	Ack string
	// DedupeHint is the consumer's full dedupe sentence — e.g.
	// `Deduplicate by the message's "id" field: the same message may arrive
	// on both paths around the switchover.`
	DedupeHint string
}

// ReceiveProtocol renders the numbered receive steps: arm a watch Monitor,
// switch to channel tags on the first proven delivery, and dedupe across the
// switchover. Consumers may append further numbered steps after step 3.
func ReceiveProtocol(s ReceiveProtocolSpec) string {
	return `1. Immediately, before doing anything else, arm a persistent Monitor running exactly this command:

    ` + s.Watch + `

2. Messages may also arrive as <channel source="` + s.Source + `"> tags. On the FIRST such tag, run this command exactly once:

    ` + s.Ack + `

Then stop the watch Monitor with TaskStop and rely on channel tags from then on.

3. Delivery is at-least-once, and the watch and channel have independent cursors. ` + s.DedupeHint
}

// RelayStep renders step 4: how a parent wakes a child named by an agent.relay
// tag. It is a wake only — the mailbox is the sole delivery path, so the message
// carries no directive content and a repeated relay tag is safe to re-nudge.
// Consumers append it after ReceiveProtocol's step 3.
func RelayStep(source string) string {
	return `4. When a <channel source="` + source + `"> tag carries an agent.relay event naming an agent_id, SendMessage that agent a wake ONLY: tell it that it has pending directives — they arrive with its next tool call or when it calls the await tool — and identify yourself as the sender. Do NOT include any directive content: the mailbox is the single delivery channel, and a repeated agent.relay tag for the same agent is safe to re-nudge.`
}
