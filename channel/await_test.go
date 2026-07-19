package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

type directivesReply struct {
	Directives []agent.Directive `json:"directives"`
}

func awaitPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	return port
}

func awaitSpec(port int, timeout time.Duration) AwaitSpec {
	return AwaitSpec{
		Resolve: func(context.Context) (string, int, error) { return "subj-1", port, nil },
		Timeout: timeout,
	}
}

func setAwaitChunk(t *testing.T, d time.Duration) {
	t.Helper()
	prev := awaitChunk
	awaitChunk = d
	t.Cleanup(func() { awaitChunk = prev })
}

func writeDirectives(t *testing.T, w http.ResponseWriter, directives []agent.Directive) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(directivesReply{Directives: directives}); err != nil {
		t.Errorf("encode directives: %v", err)
	}
}

func TestAwaitToolDirectives(t *testing.T) {
	directives := []agent.Directive{
		{ID: 7, Origin: "human", Text: "look at the auth bug"},
		{ID: 8, Origin: "agent-b", Text: "then update the tests"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/agents/await" {
			t.Errorf("path = %q, want /agents/await", got)
		}
		if got := r.URL.Query().Get("subject"); got != "subj-1" {
			t.Errorf("subject = %q, want subj-1", got)
		}
		if got := r.URL.Query().Get("agent"); got != "child-1" {
			t.Errorf("agent = %q, want child-1", got)
		}
		writeDirectives(t, w, directives)
	}))
	defer srv.Close()

	tool := NewAwaitTool(awaitSpec(awaitPort(t, srv), time.Minute))
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"child-1"}`), func(string) {})
	if isErr {
		t.Fatalf("isErr = true, text = %q", text)
	}
	want := "Steering channel — directives addressed to you:\n\n[human #7] look at the auth bug\n\n[agent-b #8] then update the tests"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestAwaitToolNoDirective(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	setAwaitChunk(t, 10*time.Millisecond)
	tool := NewAwaitTool(awaitSpec(awaitPort(t, srv), 40*time.Millisecond))
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"child-1"}`), func(string) {})
	if isErr {
		t.Fatalf("isErr = true, text = %q", text)
	}
	if want := "no directive — re-park with await or continue"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestAwaitToolProgressBetweenChunks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusNoContent) // first chunk parks empty, spanning a boundary
			return
		}
		writeDirectives(t, w, []agent.Directive{{ID: 1, Origin: "human", Text: "go"}})
	}))
	defer srv.Close()

	setAwaitChunk(t, 10*time.Millisecond)
	var progressCalls atomic.Int32
	tool := NewAwaitTool(awaitSpec(awaitPort(t, srv), time.Minute))
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"child-1"}`),
		func(string) { progressCalls.Add(1) })
	if isErr {
		t.Fatalf("isErr = true, text = %q", text)
	}
	if got := progressCalls.Load(); got < 1 {
		t.Fatalf("progress calls = %d, want >= 1", got)
	}
	if !strings.Contains(text, "[human #1] go") {
		t.Fatalf("text = %q, want directive block", text)
	}
}

func TestAwaitToolResolveError(t *testing.T) {
	tool := NewAwaitTool(AwaitSpec{
		Resolve: func(context.Context) (string, int, error) { return "", 0, errors.New("daemon down") },
		Timeout: time.Minute,
	})
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"child-1"}`), func(string) {})
	if !isErr {
		t.Fatalf("isErr = false, text = %q", text)
	}
	if !strings.Contains(text, "daemon down") {
		t.Fatalf("text = %q, want wrapped resolve error", text)
	}
}

func TestAwaitToolHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tool := NewAwaitTool(awaitSpec(awaitPort(t, srv), time.Minute))
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"child-1"}`), func(string) {})
	if !isErr {
		t.Fatalf("isErr = false, text = %q", text)
	}
	if !strings.Contains(text, "500") {
		t.Fatalf("text = %q, want status 500", text)
	}
}

func TestAwaitToolMissingAgentID(t *testing.T) {
	tool := NewAwaitTool(awaitSpec(0, time.Minute))
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{}`), func(string) {})
	if !isErr {
		t.Fatalf("isErr = false, text = %q", text)
	}
	if !strings.Contains(text, "agent_id is required") {
		t.Fatalf("text = %q, want agent_id required", text)
	}
}

func TestAwaitToolCtxCancel(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // block until the client cancels
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	tool := NewAwaitTool(awaitSpec(awaitPort(t, srv), time.Minute))

	type result struct {
		text  string
		isErr bool
	}
	done := make(chan result, 1)
	go func() {
		text, isErr := tool.Handler(ctx, json.RawMessage(`{"agent_id":"child-1"}`), func(string) {})
		done <- result{text, isErr}
	}()

	<-started
	cancel()
	select {
	case res := <-done:
		if !res.isErr {
			t.Fatalf("isErr = false, text = %q", res.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return promptly after ctx cancel")
	}
}

func TestAwaitToolHugeTimeoutClamped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"directives":[{"ID":1,"Origin":"human","Text":"hi"}]}`))
	}))
	defer srv.Close()
	tool := NewAwaitTool(awaitSpec(awaitPort(t, srv), time.Second))
	text, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"a1","timeout_seconds":9223372037}`), func(string) {})
	if isErr {
		t.Fatalf("huge timeout errored: %s", text)
	}
	if !strings.Contains(text, "[human #1] hi") {
		t.Fatalf("expected directive delivery, got %q", text)
	}
}
