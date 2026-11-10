package daemon

import (
	"os"
	"strconv"

	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/cc-interact/procs"
	"github.com/yasyf/daemonkit/durable"
)

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

// ChannelConsumer is the stream-consumer name the channel server registers under.
const ChannelConsumer = "channel"

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
	case attached && s.proven(pid):
		return ChannelActive
	case attached:
		s.InjectEvent(subjectID, consumer, pid, probePayload)
		return ChannelPending
	case s.activity.PolledSince(hc.Scope, consumer, pid, channelPollWindow):
		return ChannelPending
	}
	return ChannelInactive
}

// recordProof persists that window pid's model acked a channel tag, naming the
// process by its start time so a recycled pid never inherits the proof.
func (s *Server) recordProof(pid int) error {
	start, err := procs.StartTime(pid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(statepath.WindowProofs(s.paths), 0o700); err != nil {
		return err
	}
	return durable.WriteFile(statepath.WindowProof(s.paths, pid), []byte(strconv.FormatInt(start, 10)), 0o600)
}

// proven reports whether window pid's model has acked a channel tag while the
// same process still holds the pid. Proof is a fact about the window, recorded
// on disk, so a daemon restart never asks the model again.
func (s *Server) proven(pid int) bool {
	recorded, err := os.ReadFile(statepath.WindowProof(s.paths, pid))
	if err != nil {
		return false
	}
	start, err := procs.StartTime(pid)
	return err == nil && string(recorded) == strconv.FormatInt(start, 10)
}
