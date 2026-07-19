// Package daemon is cc-interact's lazily-started local daemon. Daemonkit owns
// its lifecycle, exact v4 persistent transport, peer identity, admission, and
// takeover; this package owns only cc-interact operations and dispatch policy.
package daemon

import "encoding/json"

// Op is a cc-interact business operation.
type Op string

// Core business operations registered by New. Consumers may add domain ops.
const (
	OpResolve        Op = "resolve"
	OpSessionRecord  Op = "session-record"
	OpGuardEdit      Op = "guard-edit"
	OpChannelAck     Op = "channel-ack"
	OpStatus         Op = "status"
	OpAgentStart     Op = "agent-start"
	OpAgentStop      Op = "agent-stop"
	OpAgentInject    Op = "agent-inject"
	OpAgentReport    Op = "agent-report"
	OpAgentDirect    Op = "agent-direct"
	OpAgentReconcile Op = "agent-reconcile"
)

// Envelope is one cc-interact operation payload. Op selects the v4 wire route
// and is never duplicated inside the payload.
type Envelope struct {
	Op        Op              `json:"-"`
	Session   string          `json:"session,omitempty"`
	ClaudePID int             `json:"claude_pid,omitempty"`
	Scope     string          `json:"scope,omitempty"`
	Consumer  string          `json:"consumer,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// Reply is one cc-interact business response.
type Reply struct {
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
