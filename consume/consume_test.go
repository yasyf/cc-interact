package consume

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/daemonkit/paths"
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

func TestStreamURLExcludeOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origin  event.Origin
		present bool
	}{
		{"set origin rides the URL", event.OriginAgent, true},
		{"zero value omits the param", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := streamURL(StreamSource{Port: 1, SubjectID: "r", Consumer: "watch", ExcludeOrigin: tc.origin})
			if tc.present && !strings.Contains(u, "exclude_origin="+string(tc.origin)) {
				t.Fatalf("url %q missing exclude_origin=%s", u, tc.origin)
			}
			if !tc.present && strings.Contains(u, "exclude_origin") {
				t.Fatalf("url %q must not carry exclude_origin for the zero value", u)
			}
		})
	}
}

// TestConsumeEventsSkipsCaughtUpMarker proves the SSE caught-up marker — a named
// event carrying a seq payload and no id — is transparent to consumers: it is
// never delivered as an event, never errors the stream, and never advances the
// cursor past the real events around it.
func TestConsumeEventsSkipsCaughtUpMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}
	if err := statepath.EnsureSubjectDir(p, "caught-up"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"comment.created\"}\n\n")
		fmt.Fprint(w, "event: caught-up\ndata: {\"seq\":1}\n\n")
		fmt.Fprint(w, "id: 2\ndata: {\"type\":\"submit\"}\n\n")
	}))
	t.Cleanup(srv.Close)

	src := StreamSource{Port: ssePort(t, srv), SubjectID: "caught-up", Consumer: "watch", Paths: p}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	if err := ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
		got = append(got, eventType(data))
		return eventType(data) == "submit", nil
	}); err != nil {
		t.Fatalf("ConsumeEvents: %v", err)
	}
	if len(got) != 2 || got[0] != "comment.created" || got[1] != "submit" {
		t.Fatalf("delivered %v, want [comment.created submit] (caught-up must be transparent)", got)
	}
	cursor, err := readCursor(statepath.Cursor(p, "caught-up", "watch"))
	if err != nil {
		t.Fatalf("readCursor: %v", err)
	}
	if cursor != 2 {
		t.Fatalf("persisted cursor = %d, want 2", cursor)
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

func TestWriteCursorConcurrent(t *testing.T) {
	const (
		writers    = 32
		iterations = 50
	)
	path := filepath.Join(t.TempDir(), "watch.cursor")
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for iteration := range iterations {
				cursor := int64(writer*iterations + iteration + 1)
				if err := writeCursor(path, cursor); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("writeCursor: %v", err)
	}
	cursor, err := readCursor(path)
	if err != nil {
		t.Fatalf("read final cursor: %v", err)
	}
	if cursor <= 0 {
		t.Fatalf("final cursor = %d, want a positive parsed value", cursor)
	}
}

func TestConsumeEventsWarnsAndContinuesAfterCursorPersistFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}
	const subjectID = "persist-warning"
	dir := statepath.SubjectDir(p, subjectID)
	prepared := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := os.RemoveAll(dir); err != nil {
			prepared <- err
			return
		}
		if err := os.WriteFile(dir, []byte("block cursor directory"), 0o600); err != nil {
			prepared <- err
			return
		}
		prepared <- nil
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "id: 6\ndata: {\"type\":\"comment.created\"}\n\n")
		_, _ = fmt.Fprint(w, "id: 7\ndata: {\"type\":\"submit\"}\n\n")
	}))
	t.Cleanup(srv.Close)

	warnings := make(chan error, 2)
	src := StreamSource{
		Port: ssePort(t, srv), SubjectID: subjectID, Consumer: "watch", Paths: p,
		Warn: func(err error) { warnings <- err },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var delivered []string
	if err := ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
		typeName := eventType(data)
		delivered = append(delivered, typeName)
		return typeName == "submit", nil
	}); err != nil {
		t.Fatalf("ConsumeEvents: %v", err)
	}
	if err := <-prepared; err != nil {
		t.Fatalf("plant cursor blocker: %v", err)
	}
	if len(delivered) != 2 || delivered[0] != "comment.created" || delivered[1] != "submit" {
		t.Fatalf("delivered = %v, want [comment.created submit]", delivered)
	}
	for range 2 {
		select {
		case err := <-warnings:
			if !strings.Contains(err.Error(), "persist cursor") {
				t.Fatalf("warning = %v, want persist cursor error", err)
			}
		default:
			t.Fatal("Warn was not invoked for each cursor persist failure")
		}
	}
}

func TestSeedCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "furthest cursor seeds before dead sibling GC",
			run: func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				p := paths.Paths{App: ".cc-interact-test"}
				const subjectID = "seeded"
				if err := statepath.EnsureSubjectDir(p, subjectID); err != nil {
					t.Fatal(err)
				}
				const deadPID = int32(1<<31 - 1)
				exists, err := process.PidExists(deadPID)
				if err != nil {
					t.Fatalf("check dead test pid: %v", err)
				}
				if exists {
					t.Skipf("test pid %d unexpectedly exists", deadPID)
				}
				self := fmt.Sprintf("watch-%d", os.Getppid())
				liveSibling := fmt.Sprintf("watch-%d", os.Getpid())
				deadSibling := fmt.Sprintf("watch-%d", deadPID)
				for consumer, cursor := range map[string]int64{
					"watch": 5, liveSibling: 7, deadSibling: 9,
				} {
					if err := writeCursor(statepath.Cursor(p, subjectID, consumer), cursor); err != nil {
						t.Fatalf("write %s cursor: %v", consumer, err)
					}
				}
				if err := SeedCursor(p, subjectID, "watch", self); err != nil {
					t.Fatalf("SeedCursor: %v", err)
				}
				seeded, err := readCursor(statepath.Cursor(p, subjectID, self))
				if err != nil || seeded != 9 {
					t.Fatalf("own cursor = %d, %v; want 9, nil", seeded, err)
				}
				if _, err := os.Stat(statepath.Cursor(p, subjectID, deadSibling)); !os.IsNotExist(err) {
					t.Fatalf("dead sibling still exists: %v", err)
				}
				for consumer, want := range map[string]int64{"watch": 5, liveSibling: 7} {
					got, err := readCursor(statepath.Cursor(p, subjectID, consumer))
					if err != nil || got != want {
						t.Fatalf("kept %s cursor = %d, %v; want %d, nil", consumer, got, err, want)
					}
				}
			},
		},
		{
			name: "no cursors leaves replay at zero without writing",
			run: func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				p := paths.Paths{App: ".cc-interact-test"}
				const subjectID = "empty"
				self := fmt.Sprintf("watch-%d", os.Getpid())
				if err := SeedCursor(p, subjectID, "watch", self); err != nil {
					t.Fatalf("SeedCursor: %v", err)
				}
				if _, err := os.Stat(statepath.Cursor(p, subjectID, self)); !os.IsNotExist(err) {
					t.Fatalf("zero seed wrote an own cursor: %v", err)
				}
			},
		},
		{
			name: "unscoped base cursor is ignored",
			run: func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				p := paths.Paths{App: ".cc-interact-test"}
				const subjectID = "base-ignored"
				if err := statepath.EnsureSubjectDir(p, subjectID); err != nil {
					t.Fatal(err)
				}
				basePath := statepath.Cursor(p, subjectID, "watch")
				if err := writeCursor(basePath, 9); err != nil {
					t.Fatal(err)
				}
				self := fmt.Sprintf("watch-%d", os.Getpid())
				if err := SeedCursor(p, subjectID, "watch", self); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(statepath.Cursor(p, subjectID, self)); !os.IsNotExist(err) {
					t.Fatalf("base cursor seeded process cursor: %v", err)
				}
				if got, err := readCursor(basePath); err != nil || got != 9 {
					t.Fatalf("base cursor changed: %d, %v", got, err)
				}
			},
		},
		{
			name: "pre-v1 cursor namespace is ignored",
			run: func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				p := paths.Paths{App: ".cc-interact-test"}
				const subjectID = "old-namespace"
				if err := p.EnsureSubjectDir(subjectID); err != nil {
					t.Fatal(err)
				}
				legacy := p.ConsumerCursorPath(subjectID, fmt.Sprintf("watch-%d", os.Getpid()))
				if err := writeCursor(legacy, 11); err != nil {
					t.Fatal(err)
				}
				self := fmt.Sprintf("watch-%d", os.Getppid())
				if err := SeedCursor(p, subjectID, "watch", self); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(statepath.Cursor(p, subjectID, self)); !os.IsNotExist(err) {
					t.Fatalf("pre-v1 cursor seeded v1 state: %v", err)
				}
				if got, err := readCursor(legacy); err != nil || got != 11 {
					t.Fatalf("pre-v1 cursor changed: %d, %v", got, err)
				}
			},
		},
		{
			name: "own corrupt cursor remains fatal",
			run: func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				p := paths.Paths{App: ".cc-interact-test"}
				const subjectID = "corrupt-own"
				if err := statepath.EnsureSubjectDir(p, subjectID); err != nil {
					t.Fatal(err)
				}
				self := fmt.Sprintf("watch-%d", os.Getpid())
				selfPath := statepath.Cursor(p, subjectID, self)
				if err := os.WriteFile(selfPath, []byte("torn"), 0o600); err != nil {
					t.Fatalf("write corrupt own cursor: %v", err)
				}
				err := SeedCursor(p, subjectID, "watch", self)
				if err == nil || !strings.Contains(err.Error(), selfPath) {
					t.Fatalf("SeedCursor error = %v, want corrupt own cursor path", err)
				}
				contents, err := os.ReadFile(selfPath)
				if err != nil || string(contents) != "torn" {
					t.Fatalf("own cursor = %q, %v; want unchanged torn value", contents, err)
				}
			},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestSeedCursorWarnsAndSkipsCorruptSibling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}
	const subjectID = "corrupt-sibling"
	if err := statepath.EnsureSubjectDir(p, subjectID); err != nil {
		t.Fatal(err)
	}
	self := fmt.Sprintf("watch-%d", os.Getppid())
	liveSibling := fmt.Sprintf("watch-%d", os.Getpid())
	if err := os.WriteFile(statepath.Cursor(p, subjectID, liveSibling), []byte("torn"), 0o600); err != nil {
		t.Fatalf("write corrupt sibling: %v", err)
	}
	var warnings []error
	if err := seedCursor(p, subjectID, "watch", self, func(err error) {
		warnings = append(warnings, err)
	}); err != nil {
		t.Fatalf("seedCursor: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), liveSibling) {
		t.Fatalf("warnings = %v, want one corrupt-sibling warning", warnings)
	}
	if _, err := os.Stat(statepath.Cursor(p, subjectID, self)); !os.IsNotExist(err) {
		t.Fatalf("corrupt sibling wrote a zero process cursor: %v", err)
	}
	if _, err := os.Stat(statepath.Cursor(p, subjectID, liveSibling)); err != nil {
		t.Fatalf("live corrupt sibling was removed: %v", err)
	}
}

// TestConsumeEventsSendsConsumerParamAndRefreshes proves the two stream-survival
// properties: the consumer name rides the SSE URL, and after the first server
// dies the Refresh hook redirects the stream to the replacement daemon. It also
// asserts the per-consumer cursor lands on the last delivered seq.
func TestConsumeEventsSendsConsumerParamAndRefreshes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := paths.Paths{App: ".cc-interact-test"}
	if err := statepath.EnsureSubjectDir(p, "stream-test"); err != nil {
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
	cursor, err := readCursor(statepath.Cursor(p, "stream-test", "watch"))
	if err != nil {
		t.Fatalf("readCursor: %v", err)
	}
	if cursor != 2 {
		t.Fatalf("persisted cursor = %d, want 2", cursor)
	}
}
