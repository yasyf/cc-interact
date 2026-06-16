package consume

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/paths"
)

func eventType(data string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	return e.Type
}

func TestStreamURLClaudePID(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		want string
	}{
		{"non-zero pid rides the URL", 4242, "&claude_pid=4242"},
		{"zero pid stays absent", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := streamURL(StreamSource{Port: 1, SubjectID: "r", Consumer: "watch", ClaudePID: tc.pid})
			if tc.want != "" && !strings.Contains(u, tc.want) {
				t.Fatalf("url %q missing %q", u, tc.want)
			}
			if tc.want == "" && strings.Contains(u, "claude_pid") {
				t.Fatalf("url %q must not carry claude_pid for pid 0", u)
			}
		})
	}
}

func ssePort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestConsumeEventsSendsConsumerParamAndRefreshes proves the two stream-survival
// properties: the consumer name rides the SSE URL, and after the first server
// dies the Refresh hook redirects the stream to the replacement daemon. It also
// asserts the per-consumer cursor lands on the last delivered seq.
func TestConsumeEventsSendsConsumerParamAndRefreshes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}
	if err := p.EnsureSubjectDir("stream-test"); err != nil {
		t.Fatal(err)
	}

	var sawConsumerA, sawConsumerB atomic.Value

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawConsumerA.Store(r.URL.Query().Get("consumer"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"comment.created\"}\n\n")
		// Returning closes the stream: the "old daemon" dying mid-session.
	}))
	t.Cleanup(a.Close)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawConsumerB.Store(r.URL.Query().Get("consumer"))
		if got := r.Header.Get("Last-Event-ID"); got != "1" {
			t.Errorf("Last-Event-ID = %q, want 1 (cursor must survive the refresh)", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 2\ndata: {\"type\":\"submit\"}\n\n")
	}))
	t.Cleanup(b.Close)

	src := StreamSource{
		Port: ssePort(t, a), SubjectID: "stream-test", Consumer: "watch", Paths: p,
		Refresh: func(context.Context) (int, error) {
			return ssePort(t, b), nil
		},
	}
	var got []string
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
		got = append(got, eventType(data))
		return eventType(data) == "submit", nil
	})
	if err != nil {
		t.Fatalf("ConsumeEvents: %v", err)
	}
	if len(got) != 2 || got[0] != "comment.created" || got[1] != "submit" {
		t.Fatalf("delivered %v, want [comment.created submit]", got)
	}
	if sawConsumerA.Load() != "watch" || sawConsumerB.Load() != "watch" {
		t.Fatalf("consumer param missing: a=%v b=%v", sawConsumerA.Load(), sawConsumerB.Load())
	}
	cursor, err := readCursor(p.ConsumerCursorPath("stream-test", "watch"))
	if err != nil {
		t.Fatalf("readCursor: %v", err)
	}
	if cursor != 2 {
		t.Fatalf("persisted cursor = %d, want 2", cursor)
	}
}
