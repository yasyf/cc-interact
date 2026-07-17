package channel

// InstructionsSpec parameterizes Instructions, the channel-instructions
// boilerplate a consumer folds into the agent's system prompt at the
// channel's MCP initialize.
type InstructionsSpec struct {
	// Desc completes "This MCP server is ..." — e.g. "the cc-review code-review channel".
	Desc string
	// Traffic is the subject-and-verb clause naming the tag traffic — e.g.
	// "Review activity reaches you" or "Orchestrator directives arrive".
	Traffic string
	// Source is the source attribute the consumer's channel renders in its
	// tags, free-form: a bare name ("cc-review") or a plugin-qualified id
	// ("plugin:cc-orchestrate:cc-orchestrate").
	Source string
	// Guide is the consumer's event guide: the paragraphs between the intro
	// and the silence closer, naming its event types and how to handle them.
	Guide string
	// SilentOutside completes "outside ... it is silent" — e.g. "a /cc-review:start run".
	SilentOutside string
}

// Instructions assembles the shared channel-instructions skeleton — intro,
// tag format, the consumer's event guide, and the silence closer — so every
// consumer's channel describes itself the same way.
func Instructions(s InstructionsSpec) string {
	return "This MCP server is " + s.Desc + ". " + s.Traffic +
		` as <channel source="` + s.Source + `" type="..."> tags whose inner JSON has a "type" field identifying the event.

` + s.Guide + `

The channel never speaks unsolicited: outside ` + s.SilentOutside + ` it is silent, and silence needs nothing from you.`
}
