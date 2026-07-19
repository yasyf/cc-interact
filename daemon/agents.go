package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
)

// agentGateBlockBudget is how many consecutive stop-gate blocks an agent may
// accrue before agent-stop forces the stop open, escaping a refuse loop.
const agentGateBlockBudget = 3

// agentStopGateInstruction is appended to the delivered directives so the child
// acts on each once and then finishes rather than looping.
const agentStopGateInstruction = "Act on each directive above exactly once, then finish your turn."

// Await long-poll bounds: a request's ?timeout is clamped to maxAwaitTimeout and
// falls back to defaultAwaitTimeout when absent or unparseable.
const (
	defaultAwaitTimeout = 30 * time.Second
	maxAwaitTimeout     = 5 * time.Minute
)

// Delivery channels recorded in an agent.delivered event's via field.
const (
	deliveredViaStopGate = "stop-gate"
	deliveredViaHook     = "hook"
	deliveredViaAwait    = "await"
)

type agentStartBody struct {
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
	ParentAgentID  string `json:"parent_agent_id"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

type agentStopBody struct {
	AgentID              string `json:"agent_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
	AgentTranscriptPath  string `json:"agent_transcript_path"`
}

type agentInjectBody struct {
	AgentID string `json:"agent_id"`
}

type agentReportBody struct {
	Session      string          `json:"session"`
	Scope        string          `json:"scope"`
	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

type agentDirectBody struct {
	AgentID string `json:"agent_id"`
	Origin  string `json:"origin"`
	Text    string `json:"text"`
}

type directivesReply struct {
	Directives []agent.Directive `json:"directives"`
}

type directReply struct {
	DirectiveID int64 `json:"directive_id"`
}

type rosterReply struct {
	Agents []agent.Info `json:"agents"`
}

type startedPayload struct {
	Type          string `json:"type"`
	AgentID       string `json:"agent_id"`
	AgentType     string `json:"agent_type"`
	ParentAgentID string `json:"parent_agent_id"`
}

type stoppedPayload struct {
	Type                 string `json:"type"`
	AgentID              string `json:"agent_id"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	AgentTranscriptPath  string `json:"agent_transcript_path,omitempty"`
}

type directedPayload struct {
	Type        string `json:"type"`
	AgentID     string `json:"agent_id"`
	Origin      string `json:"origin"`
	DirectiveID int64  `json:"directive_id"`
	Text        string `json:"text"`
}

type deliveredPayload struct {
	Type         string  `json:"type"`
	AgentID      string  `json:"agent_id"`
	Via          string  `json:"via"`
	DirectiveIDs []int64 `json:"directive_ids"`
}

type agentPayload struct {
	Type    string `json:"type"`
	AgentID string `json:"agent_id"`
}

type reportPayload struct {
	Type   string          `json:"type"`
	Report json.RawMessage `json:"report"`
}

// gateBlockCounter tracks consecutive stop-gate blocks per (subject, agent) so
// agent-stop can force the stop open after agentGateBlockBudget refusals. It is
// in-memory and reset by any non-block stop (a delivery or an allow).
type gateBlockCounter struct {
	mu     sync.Mutex
	counts map[gateBlockKey]int
}

type gateBlockKey struct {
	subjectID string
	agentID   string
}

func newGateBlockCounter() *gateBlockCounter {
	return &gateBlockCounter{counts: make(map[gateBlockKey]int)}
}

func (g *gateBlockCounter) incr(subjectID, agentID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := gateBlockKey{subjectID, agentID}
	g.counts[k]++
	return g.counts[k]
}

func (g *gateBlockCounter) reset(subjectID, agentID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.counts, gateBlockKey{subjectID, agentID})
}

func startedEvent(info agent.Info) *event.Event {
	payload, _ := json.Marshal(startedPayload{agent.EventStarted, info.AgentID, info.AgentType, info.ParentAgentID})
	return &event.Event{
		SubjectID: info.SubjectID,
		Origin:    event.OriginSystem,
		Type:      agent.EventStarted,
		Payload:   payload,
		DedupKey:  agent.EventStarted + ":" + info.AgentID,
	}
}

func stoppedEvent(subjectID string, b agentStopBody) *event.Event {
	payload, _ := json.Marshal(stoppedPayload{agent.EventStopped, b.AgentID, b.LastAssistantMessage, b.AgentTranscriptPath})
	return &event.Event{SubjectID: subjectID, Origin: event.OriginSystem, Type: agent.EventStopped, Payload: payload}
}

func directedEvent(d agent.Directive) *event.Event {
	payload, _ := json.Marshal(directedPayload{agent.EventDirected, d.AgentID, d.Origin, d.ID, d.Text})
	return &event.Event{SubjectID: d.SubjectID, Origin: d.Origin, Type: agent.EventDirected, Payload: payload}
}

func deliveredEvent(subjectID, agentID, via string, directives []agent.Directive) *event.Event {
	ids := make([]int64, len(directives))
	for i, d := range directives {
		ids[i] = d.ID
	}
	payload, _ := json.Marshal(deliveredPayload{agent.EventDelivered, agentID, via, ids})
	return &event.Event{SubjectID: subjectID, Origin: event.OriginSystem, Type: agent.EventDelivered, Payload: payload}
}

func relayEvent(subjectID, agentID string) *event.Event {
	payload, _ := json.Marshal(agentPayload{agent.EventRelay, agentID})
	return &event.Event{SubjectID: subjectID, Origin: event.OriginSystem, Type: agent.EventRelay, Payload: payload}
}

func forcedAllowEvent(subjectID, agentID string) *event.Event {
	payload, _ := json.Marshal(agentPayload{agent.EventForcedAllow, agentID})
	return &event.Event{SubjectID: subjectID, Origin: event.OriginSystem, Type: agent.EventForcedAllow, Payload: payload}
}

// stopGateReason joins the delivered directive texts and appends the fixed
// instruction, so the returned block tells the child what to do before finishing.
func stopGateReason(pending []agent.Directive) string {
	var b strings.Builder
	for i, d := range pending {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%s #%d] %s", d.Origin, d.ID, d.Text)
	}
	b.WriteString("\n\n")
	b.WriteString(agentStopGateInstruction)
	return b.String()
}

func parseAwaitTimeout(r *http.Request) time.Duration {
	v := r.URL.Query().Get("timeout")
	if v == "" {
		return defaultAwaitTimeout
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return min(d, maxAwaitTimeout)
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, maxAwaitTimeout)
	}
	return defaultAwaitTimeout
}

// Direct is the single enqueue chokepoint for a directive addressed to an agent:
// handlers, the consumer's REST surface, and the channel tools all route through
// it. It enqueues under one transaction, appends agent.directed with the
// directive's own origin (an agent-origin directive self-suppresses for
// exclude-origin=agent consumers), releases any live park, and — when the agent
// already stopped — appends agent.relay (OriginSystem) so the subject's watchers
// see the directive land on a done agent.
func (s *Server) Direct(ctx context.Context, subjectID, agentID, origin, text string) (agent.Directive, error) {
	directive, status, err := s.store.EnqueueDirective(ctx, subjectID, agentID, origin, text, time.Now())
	if err != nil {
		return agent.Directive{}, fmt.Errorf("enqueue directive: %w", err)
	}
	if _, err := s.Append(ctx, directedEvent(directive)); err != nil {
		return agent.Directive{}, fmt.Errorf("append agent.directed: %w", err)
	}
	s.parks.release(subjectID, agentID)
	if status == agent.StatusDone {
		if _, err := s.Append(ctx, relayEvent(subjectID, agentID)); err != nil {
			return agent.Directive{}, fmt.Errorf("append agent.relay: %w", err)
		}
	}
	return directive, nil
}

// reconcileDirectives re-announces directives stranded on stopped agents: for
// every done agent still holding an undelivered directive it appends agent.relay
// (OriginSystem) so the subject's watchers see it. A non-destructive peek run
// once at boot and reachable on demand; a later drain stops the re-emission, and
// there is no ticker.
//
// Top-level agents (agent_id "") have no agents row, so reconcile never sees
// them — their pending rows deliver via inject/await.
func (s *Server) reconcileDirectives(ctx context.Context) error {
	agents, err := s.store.ListPendingDirectiveAgents(ctx)
	if err != nil {
		return fmt.Errorf("reconcile directives: %w", err)
	}
	for _, info := range agents {
		if _, err := s.Append(ctx, relayEvent(info.SubjectID, info.AgentID)); err != nil {
			return fmt.Errorf("reconcile relay %s/%s: %w", info.SubjectID, info.AgentID, err)
		}
	}
	return nil
}

// handleAgentStart registers a child participant and records its start, resolving
// the subject like guard-edit (an untracked session is a no-op). RegisterAgent
// reports whether the row was newly created and the started event is deduped, so a
// re-fire duplicates neither; the greeting fires only on that first create.
func (s *Server) handleAgentStart(hc HandlerCtx) Reply {
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return errReply(err.Error())
	}
	if !ok {
		return Reply{OK: true}
	}
	var b agentStartBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return errReply(fmt.Errorf("agent-start body: %w", err).Error())
	}
	if b.AgentID == "" {
		return errReply("agent-start requires an agent_id")
	}
	info := agent.Info{
		SubjectID:      sub.ID,
		AgentID:        b.AgentID,
		ParentAgentID:  b.ParentAgentID,
		AgentType:      b.AgentType,
		SessionID:      b.SessionID,
		TranscriptPath: b.TranscriptPath,
		Status:         agent.StatusRunning,
		StartedAt:      time.Now(),
	}
	created, err := s.store.RegisterAgent(hc.Ctx, info)
	if err != nil {
		return errReply(err.Error())
	}
	if _, err := s.Append(hc.Ctx, startedEvent(info)); err != nil {
		return errReply(err.Error())
	}
	if created && s.agentGreeting != nil {
		if greeting := s.agentGreeting(info); greeting != "" {
			if _, err := s.Direct(hc.Ctx, sub.ID, b.AgentID, event.OriginSystem, greeting); err != nil {
				return errReply(err.Error())
			}
		}
	}
	return Reply{OK: true}
}

// handleAgentStop is the stop-gate. It drains the agent's mailbox first: pending
// directives are delivered as a block reply that keeps the agent running. Only an
// empty drain consults AgentGate; a gate block is returned until the loop-escape
// budget is spent, after which the stop is forced open (loud). An allowed stop
// closes the agent and records agent.stopped.
func (s *Server) handleAgentStop(hc HandlerCtx) Reply {
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return errReply(err.Error())
	}
	if !ok {
		return Reply{OK: true, Allow: true}
	}
	var b agentStopBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return errReply(fmt.Errorf("agent-stop body: %w", err).Error())
	}
	if b.AgentID == "" {
		return errReply("agent-stop requires an agent_id")
	}
	now := time.Now()
	pending, err := s.store.DrainDirectives(hc.Ctx, sub.ID, b.AgentID, now)
	if err != nil {
		return errReply(err.Error())
	}
	if len(pending) > 0 {
		s.gateBlocks.reset(sub.ID, b.AgentID)
		if _, err := s.Append(hc.Ctx, deliveredEvent(sub.ID, b.AgentID, deliveredViaStopGate, pending)); err != nil {
			return errReply(err.Error())
		}
		return Reply{OK: true, Allow: false, Reason: stopGateReason(pending)}
	}
	info, err := s.store.GetAgent(hc.Ctx, sub.ID, b.AgentID)
	if err != nil {
		return errReply(err.Error())
	}
	allow, reason := true, ""
	if s.agentGate != nil {
		allow, reason = s.agentGate(hc.Ctx, sub, info)
	}
	if !allow {
		if s.gateBlocks.incr(sub.ID, b.AgentID) < agentGateBlockBudget {
			return Reply{OK: true, Allow: false, Reason: reason}
		}
		if _, err := s.Append(hc.Ctx, forcedAllowEvent(sub.ID, b.AgentID)); err != nil {
			return errReply(err.Error())
		}
	}
	s.gateBlocks.reset(sub.ID, b.AgentID)
	closed, err := s.store.CloseAgent(hc.Ctx, sub.ID, b.AgentID, now)
	if err != nil {
		return errReply(err.Error())
	}
	if closed {
		if _, err := s.Append(hc.Ctx, stoppedEvent(sub.ID, b)); err != nil {
			return errReply(err.Error())
		}
		// A directive enqueued in the drain→close gap observed the agent running (no
		// relay from Direct) and is now stranded; peek and relay so the parent sees it.
		stranded, err := s.store.HasPendingDirectives(hc.Ctx, sub.ID, b.AgentID)
		if err != nil {
			return errReply(err.Error())
		}
		if stranded {
			if _, err := s.Append(hc.Ctx, relayEvent(sub.ID, b.AgentID)); err != nil {
				return errReply(err.Error())
			}
		}
	}
	return Reply{OK: true, Allow: true}
}

// handleAgentInject drains an agent's pending directives and returns them in one
// round trip. An untracked session yields an empty slice; a non-empty drain
// records agent.delivered (via=hook).
func (s *Server) handleAgentInject(hc HandlerCtx) Reply {
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return errReply(err.Error())
	}
	if !ok {
		return directivesBody(nil)
	}
	var b agentInjectBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return errReply(fmt.Errorf("agent-inject body: %w", err).Error())
	}
	drained, err := s.store.DrainDirectives(hc.Ctx, sub.ID, b.AgentID, time.Now())
	if err != nil {
		return errReply(err.Error())
	}
	if len(drained) > 0 {
		if _, err := s.Append(hc.Ctx, deliveredEvent(sub.ID, b.AgentID, deliveredViaHook, drained)); err != nil {
			return errReply(err.Error())
		}
	}
	return directivesBody(drained)
}

// handleAgentReport records the parent's raw Task or Agent tool observation.
func (s *Server) handleAgentReport(hc HandlerCtx) Reply {
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return errReply(err.Error())
	}
	if !ok {
		return Reply{OK: true}
	}
	var b agentReportBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return errReply(fmt.Errorf("agent-report body: %w", err).Error())
	}
	typ := agent.EventResult
	marker := bytes.Contains(b.ToolResponse, []byte(`"agentId"`)) || bytes.Contains(b.ToolResponse, []byte(`"outputFile"`))
	if marker && !bytes.Contains(b.ToolResponse, []byte(`"text"`)) {
		typ = agent.EventLaunched
	}
	payload, _ := json.Marshal(reportPayload{typ, hc.Env.Body})
	if _, err := s.Append(hc.Ctx, &event.Event{
		SubjectID: sub.ID,
		Origin:    event.OriginSystem,
		Type:      typ,
		Payload:   payload,
		DedupKey:  b.ToolUseID,
	}); err != nil {
		return errReply(err.Error())
	}
	return Reply{OK: true}
}

// handleAgentDirect enqueues a directive addressed to an agent through Direct. An
// untracked session is a no-op.
func (s *Server) handleAgentDirect(hc HandlerCtx) Reply {
	sub, ok, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return errReply(err.Error())
	}
	if !ok {
		return Reply{OK: true}
	}
	var b agentDirectBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return errReply(fmt.Errorf("agent-direct body: %w", err).Error())
	}
	directive, err := s.Direct(hc.Ctx, sub.ID, b.AgentID, b.Origin, b.Text)
	if err != nil {
		return errReply(err.Error())
	}
	body, _ := json.Marshal(directReply{DirectiveID: directive.ID})
	return Reply{OK: true, Body: body}
}

func directivesBody(drained []agent.Directive) Reply {
	if drained == nil {
		drained = []agent.Directive{}
	}
	body, _ := json.Marshal(directivesReply{Directives: drained})
	return Reply{OK: true, Body: body}
}

// handleAgentAwait long-polls an agent's directives. The park is registered
// BEFORE the first drain so a Direct racing it is never lost (a later one hits the
// channel, an earlier one the drain). A non-empty drain returns 200; an empty one
// blocks for release, the timeout (a final re-drain, else 204), or cancellation.
func (s *Server) handleAgentAwait(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("subject")
	if ref == "" {
		http.Error(w, "missing subject", http.StatusBadRequest)
		return
	}
	agentID := r.URL.Query().Get("agent")
	subjectID, found, err := s.ResolveSubject(r.Context(), ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Register the park before draining so a Direct between drain and park is not lost.
	release := s.parks.wait(subjectID, agentID)
	defer s.parks.done(subjectID, agentID, release)

	drained, err := s.store.DrainDirectives(r.Context(), subjectID, agentID, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(drained) > 0 {
		s.writeAwaitDirectives(r.Context(), w, subjectID, agentID, drained)
		return
	}
	timeout := parseAwaitTimeout(r)
	select {
	case <-r.Context().Done():
		return
	case <-time.After(timeout):
		// A Direct may have landed rows and signalled release in the sliver where the
		// timeout case won the select; one final re-drain returns them, else 204.
		late, err := s.store.DrainDirectives(r.Context(), subjectID, agentID, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(late) > 0 {
			s.writeAwaitDirectives(r.Context(), w, subjectID, agentID, late)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case <-release:
		redrained, err := s.store.DrainDirectives(r.Context(), subjectID, agentID, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeAwaitDirectives(r.Context(), w, subjectID, agentID, redrained)
	}
}

// handleAgentReconcile runs the stranded-directive sweep on demand.
func (s *Server) handleAgentReconcile(hc HandlerCtx) Reply {
	if err := s.reconcileDirectives(hc.Ctx); err != nil {
		return errReply(err.Error())
	}
	return Reply{OK: true}
}

// writeAwaitDirectives records agent.delivered (via=await) for a non-empty drain
// then writes the directives as JSON. The delivered append is best effort: the
// rows are already marked delivered, so the at-most-once response ships even if
// the observability event cannot.
func (s *Server) writeAwaitDirectives(ctx context.Context, w http.ResponseWriter, subjectID, agentID string, drained []agent.Directive) {
	if drained == nil {
		drained = []agent.Directive{}
	}
	if len(drained) > 0 {
		if _, err := s.Append(ctx, deliveredEvent(subjectID, agentID, deliveredViaAwait, drained)); err != nil {
			s.log.Printf("append agent.delivered (await): %v", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(directivesReply{Directives: drained})
}

// handleAgentRoster returns a subject's agents.
func (s *Server) handleAgentRoster(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("subject")
	if ref == "" {
		http.Error(w, "missing subject", http.StatusBadRequest)
		return
	}
	subjectID, found, err := s.ResolveSubject(r.Context(), ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	agents, err := s.store.ListAgents(r.Context(), subjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agents == nil {
		agents = []agent.Info{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rosterReply{Agents: agents})
}
