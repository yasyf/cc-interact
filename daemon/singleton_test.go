package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/subject"
)

// twoHandlerSubscribe subscribes two distinct handler types to the same board
// events, so a supersede sweep for one type must leave the other's tees intact.
func twoHandlerSubscribe(_ subject.Subject, info agent.Info) []string {
	switch info.AgentType {
	case "present-handler", "watch-handler":
		return []string{"decision.created", "choice.selected"}
	}
	return nil
}

func startAgent(t *testing.T, s *Server, sub subject.Subject, agentID, agentType string) {
	t.Helper()
	body := agentStartBody{
		AgentID:        agentID,
		AgentType:      agentType,
		SessionID:      sub.SessionID,
		TranscriptPath: "/tmp/" + agentID + ".jsonl",
	}
	if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, body)); !r.OK {
		t.Fatalf("agent-start %s = %+v", agentID, r)
	}
}

func registerHandlerAt(t *testing.T, s *Server, subjectID, agentID string, started time.Time) {
	t.Helper()
	if _, err := s.store.RegisterAgent(context.Background(), agent.Info{
		SubjectID:      subjectID,
		AgentID:        agentID,
		AgentType:      "present-handler",
		SessionID:      "sess",
		TranscriptPath: "/tmp/h.jsonl",
		Status:         agent.StatusRunning,
		StartedAt:      started,
	}); err != nil {
		t.Fatalf("register handler %s: %v", agentID, err)
	}
}

func TestSingletonSupersedesSameType(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, SingletonSubscriber: true})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	startAgent(t, s, sub, "h1", "present-handler")
	startAgent(t, s, sub, "h2", "present-handler")

	if got := s.subscriptions.subscribers(sub.ID, "decision.created"); len(got) != 1 || got[0] != "h2" {
		t.Fatalf("subscribers = %v, want only the newest [h2]", got)
	}
	// Drop h1's terminal supersede directive so the tee assertion is unambiguous.
	if _, err := s.store.DrainDirectives(context.Background(), sub.ID, "h1", time.Now()); err != nil {
		t.Fatalf("drain h1: %v", err)
	}
	mustAppend(t, s, sub.ID, event.OriginHuman, "decision.created", `{"type":"decision.created"}`)
	if !pending(t, s, sub.ID, "h2") {
		t.Fatal("event was not teed to the newest subscriber")
	}
	if pending(t, s, sub.ID, "h1") {
		t.Fatal("event teed to the superseded subscriber")
	}
}

func TestSingletonSupersedeWakesParkedAwait(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, SingletonSubscriber: true})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	startAgent(t, s, sub, "h1", "present-handler")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=h1&timeout=5s", nil)
	done := make(chan struct{})
	go func() {
		s.handleAgentAwait(rec, req)
		close(done)
	}()
	waitFor(t, "h1 await parks", func() bool { return s.parks.waiting(sub.ID, "h1") })

	startAgent(t, s, sub, "h2", "present-handler") // supersedes the parked h1

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superseded await did not wake before its timeout")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("await code = %d, want 200", rec.Code)
	}
	var reply directivesReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("await body: %v", err)
	}
	if len(reply.Directives) != 1 {
		t.Fatalf("directives = %+v, want the terminal supersede directive", reply.Directives)
	}
	if d := reply.Directives[0]; d.Origin != event.OriginSupersede || d.Text != supersededDirective {
		t.Fatalf("terminal directive = {origin:%q text:%q}, want origin %q", d.Origin, d.Text, event.OriginSupersede)
	}
	if info := getAgent(t, s, sub.ID, "h1"); info.Status != agent.StatusDone {
		t.Fatalf("superseded agent status = %q, want %q", info.Status, agent.StatusDone)
	}
}

func TestSingletonLeavesOtherTypesAlone(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: twoHandlerSubscribe, SingletonSubscriber: true})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	startAgent(t, s, sub, "h1", "present-handler")
	startAgent(t, s, sub, "w1", "watch-handler")
	startAgent(t, s, sub, "h2", "present-handler") // supersedes h1 only

	got := s.subscriptions.subscribers(sub.ID, "decision.created")
	if len(got) != 2 || !hasType(got, "h2") || !hasType(got, "w1") {
		t.Fatalf("subscribers = %v, want [h2 w1]", got)
	}
	if _, err := s.store.DrainDirectives(context.Background(), sub.ID, "h1", time.Now()); err != nil {
		t.Fatalf("drain h1: %v", err)
	}
	mustAppend(t, s, sub.ID, event.OriginHuman, "decision.created", `{"type":"decision.created"}`)
	if !pending(t, s, sub.ID, "w1") {
		t.Fatal("the different-type subscriber lost its tee")
	}
	if !pending(t, s, sub.ID, "h2") {
		t.Fatal("event not teed to the newest same-type subscriber")
	}
	if pending(t, s, sub.ID, "h1") {
		t.Fatal("event teed to the superseded subscriber")
	}
}

func TestSingletonOffKeepsAllSubscribers(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe}) // knob off
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	startAgent(t, s, sub, "h1", "present-handler")
	startAgent(t, s, sub, "h2", "present-handler")

	got := s.subscriptions.subscribers(sub.ID, "decision.created")
	if len(got) != 2 || !hasType(got, "h1") || !hasType(got, "h2") {
		t.Fatalf("subscribers = %v, want both [h1 h2]", got)
	}
	mustAppend(t, s, sub.ID, event.OriginHuman, "decision.created", `{"type":"decision.created"}`)
	if !pending(t, s, sub.ID, "h1") || !pending(t, s, sub.ID, "h2") {
		t.Fatal("knob off must keep teeing to every subscriber")
	}
}

func TestSingletonSupersedeRecordsStopped(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, SingletonSubscriber: true})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	startAgent(t, s, sub, "h1", "present-handler")
	startAgent(t, s, sub, "h2", "present-handler") // supersedes h1

	stopped := eventsOfType(t, s, sub.ID, agent.EventStopped)
	if len(stopped) != 1 {
		t.Fatalf("agent.stopped events = %d, want exactly 1 (the superseded loser)", len(stopped))
	}
	var p stoppedPayload
	if err := json.Unmarshal(stopped[0].Payload, &p); err != nil {
		t.Fatalf("stopped payload: %v", err)
	}
	if p.AgentID != "h1" {
		t.Fatalf("agent.stopped for %q, want the loser h1", p.AgentID)
	}
	if info := getAgent(t, s, sub.ID, "h1"); info.Status != agent.StatusDone {
		t.Fatalf("loser status = %q, want %q", info.Status, agent.StatusDone)
	}
}

func TestSingletonSweepsUnregisteredRunningRow(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, SingletonSubscriber: true})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	// A running same-type row that never entered the in-memory registry — a
	// reconcile loser or kill-9'd zombie. Candidacy is store state, not registry.
	registerPresentHandler(t, s, sub.ID, "zombie")
	if got := s.subscriptions.subscribers(sub.ID, "decision.created"); len(got) != 0 {
		t.Fatalf("zombie already in registry: %v", got)
	}

	startAgent(t, s, sub, "h2", "present-handler") // must sweep the zombie

	if info := getAgent(t, s, sub.ID, "zombie"); info.Status != agent.StatusDone {
		t.Fatalf("zombie status = %q, want %q (swept)", info.Status, agent.StatusDone)
	}
	// The zombie's only pending directive is its terminal supersede signal, never
	// parent-relevant. Neither the supersede-time relay peek nor a boot reconcile
	// may emit an agent.relay for it.
	if got := len(eventsOfType(t, s, sub.ID, agent.EventRelay)); got != 0 {
		t.Fatalf("agent.relay events after supersede = %d, want 0", got)
	}
	if err := s.reconcileDirectives(context.Background()); err != nil {
		t.Fatalf("reconcile directives: %v", err)
	}
	if got := len(eventsOfType(t, s, sub.ID, agent.EventRelay)); got != 0 {
		t.Fatalf("agent.relay events after reconcile = %d, want 0", got)
	}
	drained, err := s.store.DrainDirectives(context.Background(), sub.ID, "zombie", time.Now())
	if err != nil {
		t.Fatalf("drain zombie: %v", err)
	}
	if len(drained) != 1 || drained[0].Origin != event.OriginSupersede || drained[0].Text != supersededDirective {
		t.Fatalf("zombie directives = %+v, want one terminal supersede directive", drained)
	}
	if got := s.subscriptions.subscribers(sub.ID, "decision.created"); len(got) != 1 || got[0] != "h2" {
		t.Fatalf("subscribers = %v, want only the survivor [h2]", got)
	}
}

func TestSingletonConcurrentRegistrationsConverge(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, SingletonSubscriber: true})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	const n = 8

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := agentStartBody{
				AgentID:        fmt.Sprintf("h%d", i),
				AgentType:      "present-handler",
				SessionID:      sub.SessionID,
				TranscriptPath: "/tmp/h.jsonl",
			}
			if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, body)); !r.OK {
				t.Errorf("agent-start h%d = %+v", i, r)
			}
		}(i)
	}
	wg.Wait()

	got := s.subscriptions.subscribers(sub.ID, "decision.created")
	if len(got) != 1 {
		t.Fatalf("live subscribers = %v, want exactly 1", got)
	}
	running := 0
	for i := 0; i < n; i++ {
		if getAgent(t, s, sub.ID, fmt.Sprintf("h%d", i)).Status == agent.StatusRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running rows = %d, want exactly 1", running)
	}
	if info := getAgent(t, s, sub.ID, got[0]); info.Status != agent.StatusRunning {
		t.Fatalf("sole subscriber %q status = %q, want %q", got[0], info.Status, agent.StatusRunning)
	}

	// Per-subject log ordering: every agent's agent.started must precede any
	// agent.stopped for it, so a seq-order consumer never sees an agent die before
	// it started.
	type marks struct{ started, stopped int64 }
	seen := map[string]*marks{}
	evs, err := s.store.EventsSince(context.Background(), sub.ID, 0, "")
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	for _, e := range evs {
		if e.Type != agent.EventStarted && e.Type != agent.EventStopped {
			continue
		}
		var p struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("%s payload: %v", e.Type, err)
		}
		m := seen[p.AgentID]
		if m == nil {
			m = &marks{}
			seen[p.AgentID] = m
		}
		if e.Type == agent.EventStarted {
			m.started = e.Seq
		} else {
			m.stopped = e.Seq
		}
	}
	for id, m := range seen {
		if m.started == 0 {
			t.Fatalf("agent %s has agent.stopped (seq %d) but no agent.started", id, m.stopped)
		}
		if m.stopped != 0 && m.stopped < m.started {
			t.Fatalf("agent %s: agent.stopped (seq %d) precedes agent.started (seq %d)", id, m.stopped, m.started)
		}
	}
}

func TestSingletonReconcileInvariant(t *testing.T) {
	type row struct {
		id      string
		started time.Time
	}
	tests := []struct {
		name string
		rows []row
		want string
	}{
		{"most recent registration wins", []row{{"h1", time.Unix(100, 0)}, {"h2", time.Unix(200, 0)}}, "h2"},
		// Register a2 first, a1 second at the same second: last-inserted (highest
		// rowid) wins, so a1 — not the lexically greater agent id — is the survivor.
		{"same-second tie breaks on last insertion (rowid), not agent id", []row{{"a2", time.Unix(100, 0)}, {"a1", time.Unix(100, 0)}}, "a1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, SingletonSubscriber: true})
			sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
			for _, r := range tt.rows {
				registerHandlerAt(t, s, sub.ID, r.id, r.started)
			}
			if err := s.reconcileSubscriptions(context.Background()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := s.subscriptions.subscribers(sub.ID, "decision.created")
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("re-derived subscribers = %v, want [%s]", got, tt.want)
			}
		})
	}
}
