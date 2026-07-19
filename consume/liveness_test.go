package consume

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/paths"
)

// TestConsumeEventsExitsWhenWindowDead proves the leaked-watcher fix: a pid-bound
// consumer whose window is already gone returns immediately without connecting or
// advancing the shared cursor.
func TestConsumeEventsExitsWhenWindowDead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"comment.created\"}\n\n")
	}))
	t.Cleanup(srv.Close)

	src := StreamSource{
		Port: ssePort(t, srv), SubjectID: "dead-window", Consumer: "watch", Paths: p,
		ClaudePID:   4242,
		WindowAlive: func(int) bool { return false },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var delivered atomic.Int32
	if err := ConsumeEvents(ctx, src, func(int64, string) (bool, error) {
		delivered.Add(1)
		return false, nil
	}); err != nil {
		t.Fatalf("ConsumeEvents: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("ConsumeEvents did not exit on its own; it parked until the deadline")
	}
	if delivered.Load() != 0 {
		t.Fatalf("delivered %d events, want 0 (a dead window must not consume)", delivered.Load())
	}
	if hits.Load() != 0 {
		t.Fatalf("connected %d times, want 0 (a dead window must not connect)", hits.Load())
	}
}

// TestConsumeEventsUnparksOnWindowDeath proves the watchdog cancels a parked
// Read mid-stream: the window is alive at connect, then dies while the consumer
// is blocked on a held-open SSE connection, and ConsumeEvents returns.
func TestConsumeEventsUnparksOnWindowDeath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}

	orig := livenessInterval
	livenessInterval = 20 * time.Millisecond
	t.Cleanup(func() { livenessInterval = orig })

	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the connection open (no events) until the request context is
		// cancelled — i.e. until the watchdog unparks this Read.
		<-r.Context().Done()
		close(released)
	}))
	t.Cleanup(srv.Close)

	// Alive on the pre-connect check (call 1), dead on every watchdog tick after.
	var calls atomic.Int32
	src := StreamSource{
		Port: ssePort(t, srv), SubjectID: "dying-window", Consumer: "watch", Paths: p,
		ClaudePID:   4242,
		WindowAlive: func(int) bool { return calls.Add(1) <= 1 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ConsumeEvents(ctx, src, func(int64, string) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("ConsumeEvents: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("ConsumeEvents did not exit; the watchdog never unparked the Read")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("server connection was never released; the parked Read was not cancelled")
	}
}

// TestConsumeEventsCreatesSubjectDir proves the cursor-persistence fix: without
// pre-creating the subject dir, ConsumeEvents creates it so the cursor persists
// and a reconnect does not replay the backlog.
func TestConsumeEventsCreatesSubjectDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}

	if _, err := os.Stat(p.SubjectDir("fresh")); !os.IsNotExist(err) {
		t.Fatalf("subject dir must not exist before ConsumeEvents: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 7\ndata: {\"type\":\"submit\"}\n\n")
	}))
	t.Cleanup(srv.Close)

	src := StreamSource{Port: ssePort(t, srv), SubjectID: "fresh", Consumer: "watch", Paths: p}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
		return eventType(data) == "submit", nil
	}); err != nil {
		t.Fatalf("ConsumeEvents: %v", err)
	}
	cursor, err := readCursor(p.ConsumerCursorPath("fresh", "watch"))
	if err != nil {
		t.Fatalf("readCursor: %v", err)
	}
	if cursor != 7 {
		t.Fatalf("persisted cursor = %d, want 7 (cursor must survive in the auto-created dir)", cursor)
	}
}

// TestWriteCursorFailsLoud proves writeCursor surfaces a persist failure rather
// than swallowing it: a path whose parent is a regular file cannot be written.
func TestWriteCursorFailsLoud(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "blocker")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCursor(filepath.Join(notADir, "cursor"), 3); err == nil {
		t.Fatal("writeCursor returned nil for an unwritable path; it must fail loud")
	}
	good := filepath.Join(dir, "good.cursor")
	if err := writeCursor(good, 9); err != nil {
		t.Fatalf("writeCursor on a valid path: %v", err)
	}
	got, err := readCursor(good)
	if err != nil || got != 9 {
		t.Fatalf("readCursor after writeCursor = %d, %v; want 9, nil", got, err)
	}
}
