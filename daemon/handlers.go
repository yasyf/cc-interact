package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/yasyf/cc-interact/subject"
)

// reserved is the set of protocol ops dispatch answers before the registry —
// health and shutdown must work across protocol versions, so they can never be
// registrations. The other core ops are ordinary registrations made in New;
// re-registering one panics as a duplicate.
var reserved = map[Op]struct{}{OpHealth: {}, OpShutdown: {}}

// HandlerCtx is everything a domain handler needs: the request, the window and
// resolved scope, the subject resolver, the database for the consumer's own
// tables, the persist→publish Append chokepoint, the live HTTP port, the
// presence registry, and the per-scope lock that serializes scope-bound captures.
type HandlerCtx struct {
	Ctx      context.Context
	Env      Envelope
	Window   subject.Window
	Scope    string
	Subjects subject.Resolver
	DB       *sql.DB
	Append   AppendFunc
	HTTPPort int
	Activity *Activity
	RepoLock *sync.Mutex
}

// HandlerFunc handles one domain op and returns its reply.
type HandlerFunc func(HandlerCtx) Reply

// Register attaches a domain handler for op. It panics on a reserved op or a
// duplicate registration — both are programmer errors caught at wiring time.
func (s *Server) Register(op Op, h HandlerFunc) {
	if _, ok := reserved[op]; ok {
		panic(fmt.Sprintf("daemon: cannot register reserved core op %q", op))
	}
	if _, ok := s.handlers[op]; ok {
		panic(fmt.Sprintf("daemon: op %q already registered", op))
	}
	s.handlers[op] = h
}

func (s *Server) handleResolve(hc HandlerCtx) Reply {
	if hc.Env.Consumer != "" {
		s.activity.NotePoll(hc.Scope, hc.Env.Consumer, hc.Env.ClaudePID)
	}
	reply := Reply{OK: true, HTTPPort: s.httpPort}
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return errReply(err.Error())
	}
	if ok {
		reply.SubjectID = sub.ID
		reply.Status = sub.Status
	}
	return reply
}

// handleSessionRecord follows session-id rotation: ids rotate on resume/clear/
// compact, so the window's open subject is rebound to the new session id here —
// this is what keeps guard-edit and status working across rotation.
func (s *Server) handleSessionRecord(hc HandlerCtx) Reply {
	if hc.Env.Session == "" {
		return Reply{OK: true}
	}
	if err := hc.Subjects.Rebind(hc.Ctx, hc.Window, hc.Scope); err != nil {
		return errReply(err.Error())
	}
	return Reply{OK: true}
}

func (s *Server) handleChannelAck(hc HandlerCtx) Reply {
	if hc.Env.ClaudePID == 0 {
		return errReply("channel-ack requires a window (no claude pid)")
	}
	s.activity.MarkProven(hc.Env.ClaudePID)
	return Reply{OK: true}
}

type statusBody struct {
	ConsumerConnected bool `json:"consumer_connected"`
}

func (s *Server) handleStatus(hc HandlerCtx) Reply {
	reply := Reply{OK: true, DaemonVersion: s.version, HTTPPort: s.httpPort}
	if sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope); err == nil && ok {
		reply.SubjectID = sub.ID
		reply.Status = sub.Status
		reply.Body, _ = json.Marshal(statusBody{ConsumerConnected: s.activity.AttachedWithin(sub.ID, attachGrace)})
	}
	return reply
}

type guardEditBody struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// handleGuardEdit is the edit-gate mechanism: resolve the subject; no subject
// means nothing to guard (allow); a resolve error fails closed (block with the
// configured reason) rather than silently permit; otherwise the injected Gate
// renders the verdict, GateObserve records it, and the reply carries Allow/Reason.
func (s *Server) handleGuardEdit(hc HandlerCtx) Reply {
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return Reply{OK: true, Allow: false, Reason: s.gateErrorReason}
	}
	if !ok {
		return Reply{OK: true, Allow: true}
	}
	var b guardEditBody
	_ = json.Unmarshal(hc.Env.Body, &b)
	tool := ToolCall{Name: b.ToolName, Input: b.ToolInput}
	allow, reason := true, ""
	if s.gate != nil {
		allow, reason = s.gate(hc.Ctx, sub, tool)
	}
	if s.gateObserve != nil {
		s.gateObserve(hc.Ctx, sub, tool, allow, reason)
	}
	return Reply{OK: true, Allow: allow, Reason: reason}
}

func errReply(msg string) Reply { return Reply{OK: false, Error: msg} }
