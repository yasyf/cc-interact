package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/subject"
)

// presentHandlerSubscribe subscribes present-handler agents to the board's human
// interaction events and nothing else.
func presentHandlerSubscribe(_ subject.Subject, info agent.Info) []string {
	if info.AgentType == "present-handler" {
		return []string{"decision.created", "choice.selected"}
	}
	return nil
}

func registerPresentHandler(t *testing.T, s *Server, subjectID, agentID string) {
	t.Helper()
	if _, err := s.store.RegisterAgent(context.Background(), agent.Info{
		SubjectID:      subjectID,
		AgentID:        agentID,
		AgentType:      "present-handler",
		SessionID:      "sess",
		TranscriptPath: "/tmp/h.jsonl",
		Status:         agent.StatusRunning,
		StartedAt:      time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("register handler %s: %v", agentID, err)
	}
}

func mustAppend(t *testing.T, s *Server, subjectID, origin, typ, payload string) {
	t.Helper()
	if _, err := s.Append(context.Background(), &event.Event{
		SubjectID: subjectID, Origin: origin, Type: typ, Payload: []byte(payload),
	}); err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
}

func pending(t *testing.T, s *Server, subjectID, agentID string) bool {
	t.Helper()
	got, err := s.store.HasPendingDirectives(context.Background(), subjectID, agentID, "")
	if err != nil {
		t.Fatalf("has pending %s: %v", agentID, err)
	}
	return got
}

func hasType(types []string, want string) bool {
	for _, got := range types {
		if got == want {
			return true
		}
	}
	return false
}

// readSSETypes opens an SSE stream and returns the type of every replayed frame up
// to the ": connected" liveness comment the handler writes after the first flush.
func readSSETypes(t *testing.T, url string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var types []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == ": connected" {
			return types
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var e struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(data), &e); err == nil && e.Type != "" {
				types = append(types, e.Type)
			}
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		t.Fatalf("scan stream: %v", err)
	}
	t.Fatal("stream ended before the liveness comment")
	return nil
}

func TestTeeSubscribedEventReleasesPark(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	start := agentStartBody{AgentID: "h1", AgentType: "present-handler", SessionID: "sess1", TranscriptPath: "/tmp/h1.jsonl"}
	if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, start)); !r.OK {
		t.Fatalf("agent-start = %+v", r)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/await?subject="+sub.ID+"&agent=h1&timeout=5s", nil)
	done := make(chan struct{})
	go func() {
		s.handleAgentAwait(rec, req)
		close(done)
	}()
	waitFor(t, "await parks", func() bool { return s.parks.waiting(sub.ID, "h1") })

	payload := `{"type":"decision.created","id":"d1"}`
	if _, err := s.Append(context.Background(), &event.Event{
		SubjectID: sub.ID, Origin: event.OriginHuman, Type: "decision.created", Payload: []byte(payload),
	}); err != nil {
		t.Fatalf("append teed event: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parked await did not return after the teed event")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("await code = %d, want 200", rec.Code)
	}
	var reply directivesReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("await body: %v", err)
	}
	if len(reply.Directives) != 1 {
		t.Fatalf("directives = %+v, want one teed directive", reply.Directives)
	}
	if d := reply.Directives[0]; d.Origin != event.OriginEvent || d.Text != payload {
		t.Fatalf("teed directive = {origin:%q text:%q}, want {origin:%q text:%q}", d.Origin, d.Text, event.OriginEvent, payload)
	}
}

func TestTeeIgnoresUnsubscribedAndStopsOnClose(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	start := agentStartBody{AgentID: "h1", AgentType: "present-handler", SessionID: "sess1", TranscriptPath: "/tmp/h1.jsonl"}
	if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStart, sub, start)); !r.OK {
		t.Fatalf("agent-start = %+v", r)
	}

	mustAppend(t, s, sub.ID, event.OriginHuman, "block.updated", `{"type":"block.updated"}`)
	if pending(t, s, sub.ID, "h1") {
		t.Fatal("unsubscribed event was teed into the mailbox")
	}

	if r := s.dispatch(context.Background(), agentEnvelope(OpAgentStop, sub, agentStopBody{AgentID: "h1"})); !r.OK || !r.Allow {
		t.Fatalf("agent-stop = %+v, want ok+allow", r)
	}
	if got := s.subscriptions.subscribers(sub.ID, "decision.created"); len(got) != 0 {
		t.Fatalf("subscription survived close: %v", got)
	}
	mustAppend(t, s, sub.ID, event.OriginHuman, "decision.created", `{"type":"decision.created"}`)
	if pending(t, s, sub.ID, "h1") {
		t.Fatal("subscribed event teed after the agent closed")
	}
}

func TestTeeExcludesAgentOrigin(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerPresentHandler(t, s, sub.ID, "h1")
	if err := s.reconcileSubscriptions(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	mustAppend(t, s, sub.ID, event.OriginAgent, "decision.created", `{"type":"decision.created","own":true}`)
	if pending(t, s, sub.ID, "h1") {
		t.Fatal("agent-origin event echo-looped into the mailbox")
	}

	mustAppend(t, s, sub.ID, event.OriginHuman, "decision.created", `{"type":"decision.created"}`)
	if !pending(t, s, sub.ID, "h1") {
		t.Fatal("human-origin subscribed event was not teed")
	}
}

func TestReconcileSubscriptionsRederivesFromStore(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerPresentHandler(t, s, sub.ID, "h1")
	registerAgent(t, s, sub.ID, "w2") // a worker, not a subscriber

	if got := s.subscriptions.subscribers(sub.ID, "decision.created"); len(got) != 0 {
		t.Fatalf("registry populated before reconcile: %v", got)
	}
	if err := s.reconcileSubscriptions(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := s.subscriptions.subscribers(sub.ID, "decision.created")
	if len(got) != 1 || got[0] != "h1" {
		t.Fatalf("re-derived subscribers = %v, want [h1]", got)
	}
}

func TestMuteFrameDecision(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, MuteConsumer: "channel"})
	var nowNanos atomic.Int64
	base := time.Unix(1000, 0)
	nowNanos.Store(base.UnixNano())
	s.parks.now = func() time.Time { return time.Unix(0, nowNanos.Load()) }

	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerPresentHandler(t, s, sub.ID, "h1")
	if err := s.reconcileSubscriptions(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	decision := event.Event{Type: "decision.created"}
	block := event.Event{Type: "block.updated"}

	if s.muteFrame(sub.ID, "channel", decision) {
		t.Fatal("muted with no presence")
	}
	s.parks.noteDrain(sub.ID, "h1")
	if !s.muteFrame(sub.ID, "channel", decision) {
		t.Fatal("subscribed type not muted for a present subscriber")
	}
	if s.muteFrame(sub.ID, "watch", decision) {
		t.Fatal("a non-channel consumer was muted")
	}
	if s.muteFrame(sub.ID, "channel", block) {
		t.Fatal("an unsubscribed type was muted")
	}

	nowNanos.Store(base.Add(subscriberPresenceWindow + time.Second).UnixNano())
	if s.muteFrame(sub.ID, "channel", decision) {
		t.Fatal("still muted after presence lapsed")
	}

	ch := s.parks.wait(sub.ID, "h1")
	if !s.muteFrame(sub.ID, "channel", decision) {
		t.Fatal("not muted while a park is held")
	}
	s.parks.done(sub.ID, "h1", ch)
	if s.muteFrame(sub.ID, "channel", decision) {
		t.Fatal("still muted after the park cleared and presence lapsed")
	}
}

func TestChannelMutingEndToEnd(t *testing.T) {
	s := newTestServer(t, Config{Subscribe: presentHandlerSubscribe, MuteConsumer: "channel"})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	registerPresentHandler(t, s, sub.ID, "h1")
	if err := s.reconcileSubscriptions(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s.parks.noteDrain(sub.ID, "h1") // presence

	mustAppend(t, s, sub.ID, event.OriginHuman, "decision.created", `{"type":"decision.created","id":"d1"}`)
	mustAppend(t, s, sub.ID, event.OriginHuman, "block.updated", `{"type":"block.updated","id":"b1"}`)

	srv := httptest.NewServer(s.sse.Handler())
	defer srv.Close()

	channelTypes := readSSETypes(t, srv.URL+"/events?session="+sub.ID+"&consumer=channel&claude_pid=42&exclude_origin=agent")
	if hasType(channelTypes, "decision.created") {
		t.Fatalf("channel received the muted decision.created: %v", channelTypes)
	}
	if !hasType(channelTypes, "block.updated") {
		t.Fatalf("channel missing the unsubscribed block.updated: %v", channelTypes)
	}

	watchTypes := readSSETypes(t, srv.URL+"/events?session="+sub.ID+"&consumer=watch&exclude_origin=agent")
	if !hasType(watchTypes, "decision.created") || !hasType(watchTypes, "block.updated") {
		t.Fatalf("watch missing events it must receive, got %v", watchTypes)
	}
}
