package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/subject"
)

func agentEnvelope(op Op, sub subject.Subject, body any) Envelope {
	raw, _ := json.Marshal(body)
	return Envelope{
		Proto:     ProtocolVersion,
		Op:        op,
		Session:   sub.SessionID,
		Scope:     sub.Scope,
		ClaudePID: sub.ClaudePID,
		Body:      raw,
	}
}

func registerAgent(t *testing.T, s *Server, subjectID, agentID string) {
	t.Helper()
	if _, err := s.store.RegisterAgent(context.Background(), agent.Info{
		SubjectID:      subjectID,
		AgentID:        agentID,
		AgentType:      "worker",
		SessionID:      "sess",
		TranscriptPath: "/tmp/t.jsonl",
		Status:         agent.StatusRunning,
		StartedAt:      time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("register agent %s: %v", agentID, err)
	}
}

func eventsOfType(t *testing.T, s *Server, subjectID, typ string) []event.Event {
	t.Helper()
	evs, err := s.store.EventsSince(context.Background(), subjectID, 0, "")
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	var out []event.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", what)
}

func getAgent(t *testing.T, s *Server, subjectID, agentID string) agent.Info {
	t.Helper()
	info, err := s.store.GetAgent(context.Background(), subjectID, agentID)
	if err != nil {
		t.Fatalf("get agent %s: %v", agentID, err)
	}
	return info
}

func TestAgentStartNoSubjectNoOp(t *testing.T) {
	s := newTestServer(t, Config{
		AgentGreeting: func(agent.Info) string { return "hello" },
	})
	r := s.dispatch(context.Background(), Envelope{
		Proto: ProtocolVersion, Op: OpAgentStart, Scope: "/untracked", Session: "nope",
		Body: json.RawMessage(`{"agent_id":"w1"}`),
	})
	if !r.OK || r.Error != "" {
		t.Fatalf("agent-start no subject = %+v, want ok no-op", r)
	}
}

func TestAgentStartRegistersAndGreets(t *testing.T) {
	var gotInfo agent.Info
	s := newTestServer(t, Config{
		AgentGreeting: func(info agent.Info) string {
			gotInfo = info
			return "you are " + info.AgentID
		},
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	body := agentStartBody{
		AgentID: "w1", AgentType: "researcher", ParentAgentID: agent.TopLevel,
		SessionID: "sess1", TranscriptPath: "/tmp/w1.jsonl",
	}
	if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, body)); !r.OK {
		t.Fatalf("agent-start = %+v, want ok", r)
	}

	got := getAgent(t, s, sub.ID, "w1")
	if got.AgentType != "researcher" || got.Status != agent.StatusRunning {
		t.Fatalf("registered agent = %+v, want researcher/running", got)
	}
	if gotInfo.AgentID != "w1" || gotInfo.SubjectID != sub.ID {
		t.Fatalf("greeting saw info = %+v, want w1 under the subject", gotInfo)
	}

	started := eventsOfType(t, s, sub.ID, agent.EventStarted)
	if len(started) != 1 || started[0].Origin != event.OriginSystem {
		t.Fatalf("started events = %+v, want one system-origin event", started)
	}
	directed := eventsOfType(t, s, sub.ID, agent.EventDirected)
	if len(directed) != 1 || directed[0].Origin != event.OriginSystem {
		t.Fatalf("directed events = %+v, want one system-origin greeting", directed)
	}

	drained, err := s.store.DrainDirectives(context.Background(), sub.ID, "w1", time.Now())
	if err != nil {
		t.Fatalf("drain greeting: %v", err)
	}
	if len(drained) != 1 || drained[0].Text != "you are w1" || drained[0].Origin != event.OriginSystem {
		t.Fatalf("greeting directive = %+v, want system-origin \"you are w1\"", drained)
	}
}

func TestAgentStartReFireDoesNotDuplicate(t *testing.T) {
	greetings := 0
	s := newTestServer(t, Config{
		AgentGreeting: func(agent.Info) string { greetings++; return "hi" },
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	body := agentStartBody{AgentID: "w1", AgentType: "worker", SessionID: "sess1"}
	for i := 0; i < 3; i++ {
		if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, body)); !r.OK {
			t.Fatalf("agent-start re-fire %d = %+v, want ok", i, r)
		}
	}
	if greetings != 1 {
		t.Fatalf("greeting fired %d times, want 1 (first registration only)", greetings)
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventStarted)); got != 1 {
		t.Fatalf("started events = %d, want 1 (deduped)", got)
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventDirected)); got != 1 {
		t.Fatalf("directed events = %d, want 1 (single greeting)", got)
	}
}

func TestAgentStopStopGateDelivers(t *testing.T) {
	s := newTestServer(t, Config{
		AgentGate: func(context.Context, subject.Subject, agent.Info) (bool, string) {
			t.Error("gate consulted despite pending directives")
			return true, ""
		},
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")
	if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, "do the thing"); err != nil {
		t.Fatalf("direct: %v", err)
	}

	r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"}))
	if !r.OK || r.Allow {
		t.Fatalf("stop with pending = %+v, want a block", r)
	}
	if !contains(r.Reason, "do the thing") || !contains(r.Reason, agentStopGateInstruction) {
		t.Fatalf("stop reason = %q, want directive text + instruction", r.Reason)
	}
	if got := getAgent(t, s, sub.ID, "w1"); got.Status != agent.StatusRunning {
		t.Fatalf("agent status = %q, want running (stop-gate keeps it alive)", got.Status)
	}
	delivered := eventsOfType(t, s, sub.ID, agent.EventDelivered)
	if len(delivered) != 1 || delivered[0].Origin != event.OriginSystem {
		t.Fatalf("delivered events = %+v, want one system-origin event", delivered)
	}
	var p deliveredPayload
	if err := json.Unmarshal(delivered[0].Payload, &p); err != nil {
		t.Fatalf("delivered payload: %v", err)
	}
	if p.Via != deliveredViaStopGate || len(p.DirectiveIDs) != 1 {
		t.Fatalf("delivered payload = %+v, want via=stop-gate with one id", p)
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventStopped)); got != 0 {
		t.Fatalf("stopped events = %d, want 0 (agent still running)", got)
	}
}

func TestAgentStopEmptyDrainCloses(t *testing.T) {
	allow := func(context.Context, subject.Subject, agent.Info) (bool, string) { return true, "" }
	for _, tc := range []struct {
		name string
		gate AgentGateFunc
	}{
		{name: "nil gate always allows", gate: nil},
		{name: "gate allows", gate: allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, Config{AgentGate: tc.gate})
			sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
			registerAgent(t, s, sub.ID, "w1")
			r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{
				AgentID: "w1", LastAssistantMessage: "bye", AgentTranscriptPath: "/tmp/final.jsonl",
			}))
			if !r.OK || !r.Allow {
				t.Fatalf("stop with empty drain = %+v, want allow", r)
			}
			if got := getAgent(t, s, sub.ID, "w1"); got.Status != agent.StatusDone {
				t.Fatalf("agent status = %q, want done", got.Status)
			}
			stopped := eventsOfType(t, s, sub.ID, agent.EventStopped)
			if len(stopped) != 1 || stopped[0].Origin != event.OriginSystem {
				t.Fatalf("stopped events = %+v, want one system-origin event", stopped)
			}
			var p stoppedPayload
			if err := json.Unmarshal(stopped[0].Payload, &p); err != nil {
				t.Fatalf("stopped payload: %v", err)
			}
			if p.LastAssistantMessage != "bye" || p.AgentTranscriptPath != "/tmp/final.jsonl" {
				t.Fatalf("stopped payload = %+v, want the hook's last message + transcript", p)
			}
		})
	}
}

func TestAgentStopForcedAllowAfterBudget(t *testing.T) {
	s := newTestServer(t, Config{
		AgentGate: func(context.Context, subject.Subject, agent.Info) (bool, string) {
			return false, "keep working"
		},
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	for i := 1; i < agentGateBlockBudget; i++ {
		r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"}))
		if !r.OK || r.Allow {
			t.Fatalf("stop block %d = %+v, want a gate block", i, r)
		}
		if r.Reason != "keep working" {
			t.Fatalf("block %d reason = %q, want the gate reason", i, r.Reason)
		}
		if got := len(eventsOfType(t, s, sub.ID, agent.EventForcedAllow)); got != 0 {
			t.Fatalf("forced-allow events after block %d = %d, want 0", i, got)
		}
	}

	r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"}))
	if !r.OK || !r.Allow {
		t.Fatalf("stop at budget = %+v, want forced allow", r)
	}
	forced := eventsOfType(t, s, sub.ID, agent.EventForcedAllow)
	if len(forced) != 1 || forced[0].Origin != event.OriginSystem {
		t.Fatalf("forced-allow events = %+v, want one system-origin event", forced)
	}
	if got := getAgent(t, s, sub.ID, "w1"); got.Status != agent.StatusDone {
		t.Fatalf("agent status = %q, want done after forced allow", got.Status)
	}
}

func TestAgentStopGateCounterResetsOnDelivery(t *testing.T) {
	s := newTestServer(t, Config{
		AgentGate: func(context.Context, subject.Subject, agent.Info) (bool, string) {
			return false, "keep working"
		},
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	// Two gate blocks accrue the counter to just under the budget.
	for i := 1; i < agentGateBlockBudget; i++ {
		if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"})); r.Allow {
			t.Fatalf("pre-reset block %d = %+v, want block", i, r)
		}
	}
	// A stop-gate delivery is a non-block: it resets the consecutive-block counter.
	if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, "task"); err != nil {
		t.Fatalf("direct: %v", err)
	}
	if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"})); r.Allow {
		t.Fatalf("delivery stop = %+v, want a stop-gate block", r)
	}
	// The same number of blocks as before must not force now, proving the reset.
	for i := 1; i < agentGateBlockBudget; i++ {
		r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"}))
		if r.Allow {
			t.Fatalf("post-reset block %d = %+v, want block (counter must have reset)", i, r)
		}
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventForcedAllow)); got != 0 {
		t.Fatalf("forced-allow events = %d, want 0 (reset prevented the escape)", got)
	}
	if got := getAgent(t, s, sub.ID, "w1"); got.Status != agent.StatusRunning {
		t.Fatalf("agent status = %q, want still running", got.Status)
	}
}

func TestAgentInjectDrainsFIFO(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")
	for _, text := range []string{"a", "b", "c"} {
		if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, text); err != nil {
			t.Fatalf("direct %s: %v", text, err)
		}
	}

	r := s.dispatch(context.Background(), agentEnvelope(OpAgentInject, sub, agentInjectBody{AgentID: "w1"}))
	if !r.OK {
		t.Fatalf("inject = %+v, want ok", r)
	}
	var reply directivesReply
	if err := json.Unmarshal(r.Body, &reply); err != nil {
		t.Fatalf("inject body: %v", err)
	}
	if len(reply.Directives) != 3 {
		t.Fatalf("drained %d directives, want 3", len(reply.Directives))
	}
	for i, want := range []string{"a", "b", "c"} {
		if reply.Directives[i].Text != want {
			t.Fatalf("directive %d = %q, want %q (FIFO)", i, reply.Directives[i].Text, want)
		}
	}
	delivered := eventsOfType(t, s, sub.ID, agent.EventDelivered)
	if len(delivered) != 1 || delivered[0].Origin != event.OriginSystem {
		t.Fatalf("delivered events = %+v, want one system-origin event", delivered)
	}
	var p deliveredPayload
	if err := json.Unmarshal(delivered[0].Payload, &p); err != nil {
		t.Fatalf("delivered payload: %v", err)
	}
	if p.Via != deliveredViaHook {
		t.Fatalf("delivered via = %q, want hook", p.Via)
	}

	r2 := s.dispatch(context.Background(), agentEnvelope(OpAgentInject, sub, agentInjectBody{AgentID: "w1"}))
	var empty directivesReply
	if err := json.Unmarshal(r2.Body, &empty); err != nil {
		t.Fatalf("second inject body: %v", err)
	}
	if empty.Directives == nil || len(empty.Directives) != 0 {
		t.Fatalf("second inject = %+v, want non-nil empty slice", empty.Directives)
	}
}

func TestDirectToDoneAgentEmitsRelay(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")
	if _, err := s.store.CloseAgent(context.Background(), sub.ID, "w1", time.Unix(200, 0)); err != nil {
		t.Fatalf("close agent: %v", err)
	}

	if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, "late"); err != nil {
		t.Fatalf("direct to done agent: %v", err)
	}
	directed := eventsOfType(t, s, sub.ID, agent.EventDirected)
	if len(directed) != 1 || directed[0].Origin != event.OriginHuman {
		t.Fatalf("directed events = %+v, want one human-origin event", directed)
	}
	relay := eventsOfType(t, s, sub.ID, agent.EventRelay)
	if len(relay) != 1 || relay[0].Origin != event.OriginSystem {
		t.Fatalf("relay events = %+v, want one system-origin event", relay)
	}
	drained, err := s.store.DrainDirectives(context.Background(), sub.ID, "w1", time.Now())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(drained) != 1 || drained[0].Text != "late" {
		t.Fatalf("pending on done agent = %+v, want the queued directive", drained)
	}
}

func TestAgentAwaitDrainFirst(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")
	if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, "ready"); err != nil {
		t.Fatalf("direct: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=w1&timeout=5s", nil)
	s.handleAgentAwait(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("await drain-first code = %d, want 200", rec.Code)
	}
	var reply directivesReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("await body: %v", err)
	}
	if len(reply.Directives) != 1 || reply.Directives[0].Text != "ready" {
		t.Fatalf("await directives = %+v, want the queued directive", reply.Directives)
	}
}

func TestAgentAwaitTimeout(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=w1&timeout=40ms", nil)
	start := time.Now()
	s.handleAgentAwait(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("await timeout code = %d, want 204", rec.Code)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("await returned after %v, want it to block for the timeout", elapsed)
	}
}

func TestAgentAwaitReleasedByDirect(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=w1&timeout=5s", nil)
	done := make(chan struct{})
	go func() {
		s.handleAgentAwait(rec, req)
		close(done)
	}()

	waitFor(t, "await parks", func() bool { return s.parks.waiting(sub.ID, "w1") })
	if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, "go"); err != nil {
		t.Fatalf("direct: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parked await did not return after Direct")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("released await code = %d, want 200", rec.Code)
	}
	var reply directivesReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("await body: %v", err)
	}
	if len(reply.Directives) != 1 || reply.Directives[0].Text != "go" {
		t.Fatalf("released await directives = %+v, want the directive that released it", reply.Directives)
	}
}

func TestAgentRosterListsAgents(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")
	registerAgent(t, s, sub.ID, "w2")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents?subject="+sub.ID, nil)
	s.handleAgentRoster(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("roster code = %d, want 200", rec.Code)
	}
	var reply rosterReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("roster body: %v", err)
	}
	if len(reply.Agents) != 2 {
		t.Fatalf("roster = %d agents, want 2", len(reply.Agents))
	}
}

func TestReconcileDirectives(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")

	// A done agent holding a pending directive — the reconcile target. Direct runs
	// while it is still running, so it emits no relay yet.
	registerAgent(t, s, sub.ID, "done-pending")
	if _, err := s.Direct(context.Background(), sub.ID, "done-pending", event.OriginHuman, "recover me"); err != nil {
		t.Fatalf("direct done-pending: %v", err)
	}
	if _, err := s.store.CloseAgent(context.Background(), sub.ID, "done-pending", time.Unix(200, 0)); err != nil {
		t.Fatalf("close done-pending: %v", err)
	}
	// A running agent with a pending directive — reconcile must skip it.
	registerAgent(t, s, sub.ID, "running-pending")
	if _, err := s.Direct(context.Background(), sub.ID, "running-pending", event.OriginHuman, "keep"); err != nil {
		t.Fatalf("direct running-pending: %v", err)
	}

	if got := len(eventsOfType(t, s, sub.ID, agent.EventRelay)); got != 0 {
		t.Fatalf("relay events before reconcile = %d, want 0", got)
	}
	if err := s.reconcileDirectives(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	relays := eventsOfType(t, s, sub.ID, agent.EventRelay)
	if len(relays) != 1 {
		t.Fatalf("relay events after reconcile = %d, want 1 (done+pending only, not the running agent)", len(relays))
	}
	if relays[0].Origin != event.OriginSystem {
		t.Fatalf("relay origin = %q, want system", relays[0].Origin)
	}
	var p agentPayload
	if err := json.Unmarshal(relays[0].Payload, &p); err != nil {
		t.Fatalf("relay payload: %v", err)
	}
	if p.AgentID != "done-pending" {
		t.Fatalf("relay names agent %q, want done-pending", p.AgentID)
	}

	// Draining the stranded directive stops the re-emission.
	if _, err := s.store.DrainDirectives(context.Background(), sub.ID, "done-pending", time.Now()); err != nil {
		t.Fatalf("drain done-pending: %v", err)
	}
	if err := s.reconcileDirectives(context.Background()); err != nil {
		t.Fatalf("reconcile after drain: %v", err)
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventRelay)); got != 1 {
		t.Fatalf("relay events after drain+reconcile = %d, want 1 (no new re-emission)", got)
	}
}

// TestConcurrentDirectAndStopThroughDaemon drives Direct and agent-stop through
// the daemon dispatch path concurrently: every directive is either delivered by
// the stop-gate reply or drainable afterward — none lost, none delivered twice.
func TestConcurrentDirectAndStopThroughDaemon(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	const n = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := s.dispatch(context.Background(), agentEnvelope(OpAgentDirect, sub, agentDirectBody{
				AgentID: "w1", Origin: event.OriginHuman, Text: fmt.Sprintf("d-%d", i),
			}))
			if !r.OK {
				t.Errorf("direct %d = %+v, want ok", i, r)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"})); !r.OK {
			t.Errorf("stop = %+v, want ok", r)
		}
	}()
	close(start)
	wg.Wait()

	enqueued := map[int64]bool{}
	for _, e := range eventsOfType(t, s, sub.ID, agent.EventDirected) {
		var p directedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("directed payload: %v", err)
		}
		enqueued[p.DirectiveID] = true
	}
	if len(enqueued) != n {
		t.Fatalf("enqueued %d directives, want %d", len(enqueued), n)
	}

	delivered := map[int64]bool{}
	for _, e := range eventsOfType(t, s, sub.ID, agent.EventDelivered) {
		var p deliveredPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("delivered payload: %v", err)
		}
		for _, id := range p.DirectiveIDs {
			if delivered[id] {
				t.Fatalf("directive %d delivered twice", id)
			}
			delivered[id] = true
		}
	}

	final := getAgent(t, s, sub.ID, "w1")
	drained, err := s.store.DrainDirectives(context.Background(), sub.ID, "w1", time.Now())
	if err != nil {
		t.Fatalf("final drain: %v", err)
	}
	pendingIDs := map[int64]bool{}
	for _, d := range drained {
		if delivered[d.ID] {
			t.Fatalf("directive %d both delivered and pending — double delivery", d.ID)
		}
		pendingIDs[d.ID] = true
	}
	for id := range enqueued {
		if !delivered[id] && !pendingIDs[id] {
			t.Fatalf("directive %d neither delivered nor pending — lost", id)
		}
	}
	if len(delivered)+len(pendingIDs) != n {
		t.Fatalf("delivered %d + pending %d = %d, want %d", len(delivered), len(pendingIDs), len(delivered)+len(pendingIDs), n)
	}
	// The real invariant: a directive left pending on a STOPPED agent must have
	// triggered a relay so the parent is notified — a drain alone would mask that.
	relays := len(eventsOfType(t, s, sub.ID, agent.EventRelay))
	if final.Status == agent.StatusDone && len(pendingIDs) > 0 && relays == 0 {
		t.Fatalf("%d directives stranded on the stopped agent with no relay emitted", len(pendingIDs))
	}
}

// TestAgentStopStrandedDirectiveEmitsRelay deterministically reproduces the
// drain→close window: the gate hook (called between the empty drain and the close)
// enqueues a directive that observes the agent still running, so Direct emits no
// relay. The post-close peek must then emit one so the directive is not stranded.
func TestAgentStopStrandedDirectiveEmitsRelay(t *testing.T) {
	var s *Server
	enqueued := false
	s = newTestServer(t, Config{
		AgentGate: func(ctx context.Context, _ subject.Subject, info agent.Info) (bool, string) {
			if !enqueued {
				enqueued = true
				if _, err := s.Direct(ctx, info.SubjectID, info.AgentID, event.OriginHuman, "stranded"); err != nil {
					t.Errorf("gate-gap direct: %v", err)
				}
			}
			return true, ""
		},
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"}))
	if !r.OK || !r.Allow {
		t.Fatalf("stop = %+v, want allow", r)
	}
	relays := eventsOfType(t, s, sub.ID, agent.EventRelay)
	if len(relays) != 1 || relays[0].Origin != event.OriginSystem {
		t.Fatalf("relay events = %+v, want one system-origin relay for the stranded directive", relays)
	}
	var p agentPayload
	if err := json.Unmarshal(relays[0].Payload, &p); err != nil {
		t.Fatalf("relay payload: %v", err)
	}
	if p.AgentID != "w1" {
		t.Fatalf("relay names %q, want w1", p.AgentID)
	}
	if got := getAgent(t, s, sub.ID, "w1").Status; got != agent.StatusDone {
		t.Fatalf("agent status = %q, want done", got)
	}
}

func TestAgentAwaitDoubleAwaitWakesFirst(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=w1&timeout=5s", nil)
	done1 := make(chan struct{})
	go func() { s.handleAgentAwait(rec1, req1); close(done1) }()
	waitFor(t, "first await parks", func() bool { return s.parks.waiting(sub.ID, "w1") })

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=w1&timeout=5s", nil)
	done2 := make(chan struct{})
	go func() { s.handleAgentAwait(rec2, req2); close(done2) }()

	// The second await displaces the first, which must wake at once — not stall.
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("displaced await stalled instead of waking")
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("displaced await code = %d, want 200 (empty re-drain)", rec1.Code)
	}

	// Release the second so it does not wait out its own timeout.
	if _, err := s.Direct(context.Background(), sub.ID, "w1", event.OriginHuman, "go"); err != nil {
		t.Fatalf("direct: %v", err)
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("second await did not return after Direct")
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("second await code = %d, want 200", rec2.Code)
	}
}

func TestAgentStartConcurrentFirstRegistrationGreetsOnce(t *testing.T) {
	var greetings int32
	s := newTestServer(t, Config{
		AgentGreeting: func(agent.Info) string {
			atomic.AddInt32(&greetings, 1)
			return "hi"
		},
	})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	body := agentStartBody{AgentID: "w1", AgentType: "worker", SessionID: "sess1"}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, body)); !r.OK {
				t.Errorf("agent-start = %+v, want ok", r)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&greetings); got != 1 {
		t.Fatalf("greeting fired %d times, want exactly 1", got)
	}
	drained, err := s.store.DrainDirectives(context.Background(), sub.ID, "w1", time.Now())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("greeting directives = %d, want exactly 1", len(drained))
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventStarted)); got != 1 {
		t.Fatalf("started events = %d, want 1 (deduped)", got)
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventDirected)); got != 1 {
		t.Fatalf("directed events = %d, want 1 (single greeting)", got)
	}
}

func TestAgentStopConcurrentEmitsOneStopped(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "w1"})); !r.OK {
				t.Errorf("agent-stop = %+v, want ok", r)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := len(eventsOfType(t, s, sub.ID, agent.EventStopped)); got != 1 {
		t.Fatalf("stopped events = %d, want exactly 1 (running→done is the dedup)", got)
	}
	if got := getAgent(t, s, sub.ID, "w1").Status; got != agent.StatusDone {
		t.Fatalf("agent status = %q, want done", got)
	}
}

func TestAgentEnvelopesRejectEmptyAgentID(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")

	for _, op := range []Op{OpAgentStart, OpAgentStop, OpAgentInject} {
		r := s.dispatch(context.Background(), agentEnvelope(op, sub, map[string]string{"agent_id": ""}))
		if r.OK || !contains(r.Error, "agent_id") {
			t.Fatalf("%s with empty agent_id = %+v, want an agent_id error", op, r)
		}
	}
	// agent-direct MUST still accept the top-level agent ("").
	r := s.dispatch(context.Background(), agentEnvelope(OpAgentDirect, sub, agentDirectBody{
		AgentID: agent.TopLevel, Origin: event.OriginHuman, Text: "steer top level",
	}))
	if !r.OK {
		t.Fatalf("agent-direct to top-level = %+v, want ok", r)
	}
}

func TestAgentAwaitTimeoutReDrainReturnsLateRows(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerAgent(t, s, sub.ID, "w1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=w1&timeout=200ms", nil)
	done := make(chan struct{})
	go func() { s.handleAgentAwait(rec, req); close(done) }()
	waitFor(t, "await parks", func() bool { return s.parks.waiting(sub.ID, "w1") })
	time.Sleep(30 * time.Millisecond) // let the initial drain complete first

	// Enqueue directly, bypassing Direct's park release, so ONLY the timeout-path
	// re-drain can surface the row.
	if _, _, err := s.store.EnqueueDirective(context.Background(), sub.ID, "w1", event.OriginHuman, "late", time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("await did not return")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("timeout re-drain code = %d, want 200 (late row surfaced)", rec.Code)
	}
	var reply directivesReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("await body: %v", err)
	}
	if len(reply.Directives) != 1 || reply.Directives[0].Text != "late" {
		t.Fatalf("await directives = %+v, want the late row", reply.Directives)
	}
}
