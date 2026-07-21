// Package event defines the append-only log entry that flows through cc-interact
// and the per-subject pub/sub wakeup that fans changes out to consumers.
package event

import (
	"encoding/json"
	"time"
)

// Origin records which side produced an event.
type Origin = string

// Origins recorded in an event's Origin field. OriginAgent is the default
// agent-side origin; a consumer is free to record a more specific producer.
const (
	OriginAgent  Origin = "agent"
	OriginHuman  Origin = "human"
	OriginSystem Origin = "system"
	// OriginEvent marks a directive teed from a subject event into an agent's
	// mailbox (an event subscription), distinguishing it from operator guidance.
	OriginEvent Origin = "event"
	// OriginSupersede marks the terminal directive delivered to a subscriber that a
	// newer same-type registration superseded, telling it to finish and exit.
	OriginSupersede Origin = "supersede"
)

// Event is one entry in a subject's append-only log, fanned out to every consumer.
type Event struct {
	SubjectID string
	Seq       int64
	Origin    string
	Type      string
	Payload   json.RawMessage
	CreatedAt time.Time
	DedupKey  string // empty => no dedup
}
