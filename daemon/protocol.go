// Package daemon is cc-interact's lazily-started local daemon: a control-plane
// unix-socket server speaking newline-delimited JSON (a generic Op + Body
// envelope, so the domain rides in the opaque Body), plus the realtime HTTP/SSE
// plane it boots. A handful of core ops are answered internally; everything
// domain-specific is a HandlerFunc the consumer Registers. Newest-wins eviction
// of a strictly older socket holder makes upgrades seamless; a flock guards
// lazy start. Every control op is fast request/response — realtime delivery is
// the SSE stream (package sse), not a blocking socket op.
package daemon

import "encoding/json"

// ProtocolVersion is stamped on every envelope. health and shutdown answer
// regardless of it (cross-version eviction depends on both); every other op
// requires an exact match.
const ProtocolVersion = 1

// Op is a control-plane request operation.
type Op string

// Core ops answered by the daemon itself. Domain ops are defined by the consumer
// and attached with Register; these names are reserved and Register panics on them.
const (
	OpHealth        Op = "health"         // liveness + version probe
	OpShutdown      Op = "shutdown"       // step down and release the socket
	OpResolve       Op = "resolve"        // look up an existing subject (no create) for a stream consumer
	OpSessionRecord Op = "session-record" // record SessionStart hook facts (session rotation rebind)
	OpGuardEdit     Op = "guard-edit"     // PreToolUse: ask the gate whether an edit is permitted
	OpChannelAck    Op = "channel-ack"    // the model proves the window's channel round trip
	OpStatus        Op = "status"         // daemon version + subject status
)

// Envelope is one control-plane RPC. The generic fields the daemon itself reads
// are first-class; everything domain-specific rides in Body, which the consumer's
// handler unmarshals.
type Envelope struct {
	Proto     int             `json:"proto"`
	Op        Op              `json:"op"`
	Session   string          `json:"session,omitempty"`
	ClaudePID int             `json:"claude_pid,omitempty"` // window identity: stamped by the client
	Scope     string          `json:"scope,omitempty"`      // raw ownership scope, run through Config.ScopeResolve
	Consumer  string          `json:"consumer,omitempty"`   // stream consumer name on resolve
	Body      json.RawMessage `json:"body,omitempty"`       // domain payload
}

// Reply is one control-plane response. The generic fields the daemon sets are
// first-class; a handler returns domain output in Body.
type Reply struct {
	Proto         int             `json:"proto"`
	OK            bool            `json:"ok"`
	Error         string          `json:"error,omitempty"`
	DaemonVersion string          `json:"daemon_version,omitempty"`
	SubjectID     string          `json:"subject_id,omitempty"`
	Status        string          `json:"status,omitempty"`
	HTTPPort      int             `json:"http_port,omitempty"`
	Allow         bool            `json:"allow,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
}
