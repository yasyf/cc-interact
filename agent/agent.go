// Package agent defines participants and directives in an agent session.
package agent

import "time"

// TopLevel addresses the session's top-level agent.
const TopLevel = ""

// Status values for Info.Status.
const (
	// StatusRunning identifies a live agent.
	StatusRunning = "running"
	// StatusDone identifies a stopped agent.
	StatusDone = "done"
)

// Event types for the agent participant plane.
const (
	// EventStarted records an agent starting.
	EventStarted = "agent.started"
	// EventStopped records an agent stopping.
	EventStopped = "agent.stopped"
	// EventDirected records a directive queued for an agent.
	EventDirected = "agent.directed"
	// EventRelay records a directive relayed to an agent.
	EventRelay = "agent.relay"
	// EventDelivered records delivery of a directive.
	EventDelivered = "agent.delivered"
	// EventLaunched records an agent launch.
	EventLaunched = "agent.launched"
	// EventResult records an agent result.
	EventResult = "agent.result"
	// EventForcedAllow records an agent gate being forced open.
	EventForcedAllow = "agent.gate.forced_allow"
)

// Info describes one agent participant in a subject.
type Info struct {
	// SubjectID identifies the subject the agent belongs to.
	SubjectID string
	// AgentID identifies the agent within the subject.
	AgentID string
	// ParentAgentID identifies the agent's parent.
	ParentAgentID string
	// AgentType identifies the agent implementation.
	AgentType string
	// SessionID identifies the agent's session.
	SessionID string
	// TranscriptPath locates the agent's transcript.
	TranscriptPath string
	// Status is the agent's lifecycle state.
	Status string
	// StartedAt is when the agent started.
	StartedAt time.Time
	// EndedAt is when the agent stopped, or zero while running.
	EndedAt time.Time
}

// Directive is one instruction addressed to an agent.
type Directive struct {
	// ID is the directive's database identifier.
	ID int64
	// SubjectID identifies the directive's subject.
	SubjectID string
	// AgentID identifies the addressed agent.
	AgentID string
	// Origin identifies who issued the directive.
	Origin string
	// Text contains the directive body.
	Text string
	// CreatedAt is when the directive was queued.
	CreatedAt time.Time
	// DeliveredAt is when the directive was delivered, or zero while pending.
	DeliveredAt time.Time
}
