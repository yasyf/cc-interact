package daemon

// ChannelState classifies a window's channel route to a subject.
type ChannelState string

const (
	// ChannelActive means the window's consumer is attached and its model has
	// acked a delivered channel tag.
	ChannelActive ChannelState = "active"
	// ChannelPending means a consumer is attached or polling but delivery is
	// unproven; Claude Code drops channel tags silently when channels are off.
	ChannelPending ChannelState = "pending"
	// ChannelInactive means no channel consumer is wired for the window.
	ChannelInactive ChannelState = "inactive"
)

// ProbeEventType is the delivery probe's event type; the model answers it with channel-ack.
const ProbeEventType = "channel.probe"

const (
	probePayload      = `{"type":"` + ProbeEventType + `","note":"delivery probe; run channel-ack; no reply needed"}`
	channelPollWindow = 2 * ResolvePollCeiling
)

// ChannelState classifies the calling window's route to subjectID and probes an
// attached, unproven one with a non-persisted frame; call it mid-turn.
func (s *Server) ChannelState(hc HandlerCtx, subjectID, consumer string) ChannelState {
	pid := hc.Env.ClaudePID
	attached := s.activity.Attached(subjectID, consumer, pid)
	switch {
	case attached && s.activity.Proven(pid):
		return ChannelActive
	case attached:
		s.InjectEvent(subjectID, consumer, pid, probePayload)
		return ChannelPending
	case s.activity.PolledSince(hc.Scope, consumer, pid, channelPollWindow):
		return ChannelPending
	}
	return ChannelInactive
}
