package sse

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yasyf/cc-interact/event"
)

const (
	presenceType = "presence.changed"
	otherType    = "thing.created"
)

type fakeBackend struct {
	mu        sync.Mutex
	refs      map[string]string        // ref (slug or id) -> canonical id
	events    map[string][]event.Event // by canonical subject id
	bus       *event.Bus
	connected bool
	attached  chan string
	detached  chan string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		refs:     map[string]string{},
		events:   map[string][]event.Event{},
		bus:      event.NewBus(),
		attached: make(chan string, 4),
		detached: make(chan string, 4),
	}
}

func (b *fakeBackend) addSubject(id string, aliases ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refs[id] = id
	for _, a := range aliases {
		b.refs[a] = id
	}
}

func (b *fakeBackend) seed(subjectID string, evs ...event.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.events[subjectID]
	for _, e := range evs {
		e.SubjectID = subjectID
		e.Seq = int64(len(cur) + 1)
		cur = append(cur, e)
	}
	b.events[subjectID] = cur
}

func (b *fakeBackend) ResolveSubject(_ context.Context, ref string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.refs[ref]
	return id, ok, nil
}

func (b *fakeBackend) EventsSince(_ context.Context, subjectID string, cursor int64, excludeAgent bool) ([]event.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []event.Event
	for _, e := range b.events[subjectID] {
		if e.Seq <= cursor {
			continue
		}
		if excludeAgent && e.Origin == event.OriginAgent {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (b *fakeBackend) Subscribe(subjectID string) (<-chan struct{}, func()) {
	return b.bus.Subscribe(subjectID)
}

func (b *fakeBackend) Attach(subjectID, consumer string, pid int) func() {
	key := subjectID + "/" + consumer + "/" + strconv.Itoa(pid)
	b.attached <- key
	return func() { b.detached <- key }
}

func (b *fakeBackend) ConsumerConnected(string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

func startServer(t *testing.T, b *fakeBackend, cfg Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(b, cfg).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// connectUntilLive opens an SSE request and blocks until the handler's
// ": connected" liveness comment, proving the handler is fully set up.
func connectUntilLive(t *testing.T, ctx context.Context, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "connected") {
			return resp
		}
	}
	t.Fatal("stream ended before liveness comment")
	return nil
}

func TestEventsRegistersNamedConsumer(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id")
	srv := startServer(t, b, Config{PresenceDebounce: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	resp := connectUntilLive(t, ctx, srv.URL+"/events?session=s1id&consumer=channel&claude_pid=4242")
	defer resp.Body.Close()

	want := "s1id/channel/4242"
	select {
	case got := <-b.attached:
		if got != want {
			t.Fatalf("attached %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer was never attached")
	}

	cancel()
	select {
	case got := <-b.detached:
		if got != want {
			t.Fatalf("detached %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer was never detached on disconnect")
	}
}

func TestEventsSlugRefAttachesUnderCanonicalID(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id", "feat-x") // slug feat-x resolves to canonical s1id
	srv := startServer(t, b, Config{PresenceDebounce: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := connectUntilLive(t, ctx, srv.URL+"/events?session=feat-x&consumer=channel")
	defer resp.Body.Close()

	// The Bus and events table key on the canonical id, so the slug must be
	// translated before any subscription. No claude_pid is a legitimate pid-less
	// consumer, registered under pid 0.
	want := "s1id/channel/0"
	select {
	case got := <-b.attached:
		if got != want {
			t.Fatalf("attached %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer was never attached")
	}
}

func TestPresenceChangeTransitionsOnAttachAndDetach(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id")
	calls := make(chan bool, 4)
	srv := startServer(t, b, Config{
		PresenceDebounce:  20 * time.Millisecond,
		PresenceEventType: presenceType,
		OnPresenceChange: func(_ context.Context, subjectID string, connected bool) {
			if subjectID != "s1id" {
				t.Errorf("presence change for %q, want s1id", subjectID)
			}
			calls <- connected
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	resp := connectUntilLive(t, ctx, srv.URL+"/events?session=s1id&consumer=channel&claude_pid=4242")
	defer resp.Body.Close()

	select {
	case got := <-calls:
		if !got {
			t.Fatalf("first attach emitted connected=%v, want true", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence change never emitted on attach")
	}

	// Detach: ConsumerConnected stays false, so after the debounce the handler
	// emits connected=false.
	cancel()
	select {
	case got := <-calls:
		if got {
			t.Fatalf("detach after debounce emitted connected=%v, want false", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence change never emitted after detach debounce")
	}
}

func TestPresenceChangeSkippedWhenStillConnected(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id")
	b.connected = true // a peer consumer is already wired, so first attach is a no-op
	calls := make(chan bool, 4)
	srv := startServer(t, b, Config{
		PresenceDebounce:  20 * time.Millisecond,
		PresenceEventType: presenceType,
		OnPresenceChange:  func(_ context.Context, _ string, connected bool) { calls <- connected },
	})

	ctx, cancel := context.WithCancel(context.Background())
	resp := connectUntilLive(t, ctx, srv.URL+"/events?session=s1id&consumer=channel&claude_pid=4242")
	resp.Body.Close()
	cancel()

	// wasConnected==true on attach, and ConsumerConnected stays true through the
	// debounce, so neither transition fires.
	select {
	case got := <-calls:
		t.Fatalf("emitted presence change %v while a peer stayed connected", got)
	case <-time.After(100 * time.Millisecond):
	}
}

type sseFrame struct {
	id   int64
	data string
}

// readFramesUntilLive collects replayed SSE frames up to the ": connected"
// liveness comment the handler writes after the first flush.
func readFramesUntilLive(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	sc := bufio.NewScanner(body)
	var frames []sseFrame
	var cur sseFrame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == ": connected":
			return frames
		case strings.HasPrefix(line, "id: "):
			n, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if err != nil {
				t.Fatalf("bad id line %q: %v", line, err)
			}
			cur.id = n
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
			frames = append(frames, cur)
			cur = sseFrame{}
		}
	}
	t.Fatalf("stream ended before liveness comment, frames so far: %+v", frames)
	return nil
}

func TestPresenceFilteredFromNamedConsumers(t *testing.T) {
	seed := func(b *fakeBackend, id string) (presenceSeq, otherSeq int64) {
		b.seed(id,
			event.Event{Origin: event.OriginSystem, Type: presenceType, Payload: []byte(`{"type":"presence.changed","connected":true}`)},
			event.Event{Origin: event.OriginHuman, Type: otherType, Payload: []byte(`{"type":"thing.created","id":"1"}`)},
		)
		return 1, 2
	}
	get := func(t *testing.T, url string) []sseFrame {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return readFramesUntilLive(t, resp.Body)
	}

	cfg := Config{PresenceDebounce: 20 * time.Millisecond, PresenceEventType: presenceType}

	cases := []struct {
		name   string
		params string
		want   []string // event types in delivery order
	}{
		{"channel consumer gets only the other event", "&consumer=channel&claude_pid=4242", []string{otherType}},
		{"watch consumer gets only the other event", "&consumer=watch", []string{otherType}},
		{"browser gets both, presence first", "", []string{presenceType, otherType}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newFakeBackend()
			b.addSubject("s1id")
			srv := startServer(t, b, cfg)
			presenceSeq, otherSeq := seed(b, "s1id")
			seqs := map[string]int64{presenceType: presenceSeq, otherType: otherSeq}

			frames := get(t, srv.URL+"/events?session=s1id"+tc.params)
			if len(frames) != len(tc.want) {
				t.Fatalf("got %d frames %+v, want types %v", len(frames), frames, tc.want)
			}
			for i, typ := range tc.want {
				if frames[i].id != seqs[typ] || !strings.Contains(frames[i].data, typ) {
					t.Fatalf("frame %d = %+v, want id %d carrying %q", i, frames[i], seqs[typ], typ)
				}
			}
		})
	}

	t.Run("reconnect with last delivered id does not redeliver the filtered tail", func(t *testing.T) {
		b := newFakeBackend()
		b.addSubject("s1id")
		srv := startServer(t, b, cfg)
		_, otherSeq := seed(b, "s1id")

		url := srv.URL + "/events?session=s1id&consumer=channel&claude_pid=4242"
		frames := get(t, url)
		if len(frames) != 1 || frames[0].id != otherSeq {
			t.Fatalf("first connect frames = %+v, want only the other event at seq %d", frames, otherSeq)
		}
		// The tail past the other event holds only the filtered presence change:
		// resuming from the last delivered id must replay nothing.
		b.seed("s1id", event.Event{Origin: event.OriginSystem, Type: presenceType, Payload: []byte(`{"type":"presence.changed","connected":false}`)})
		if frames := get(t, url+"&last_event_id="+strconv.FormatInt(otherSeq, 10)); len(frames) != 0 {
			t.Fatalf("filtered tail was redelivered: %+v", frames)
		}
	})
}

func TestEventsUnknownSubjectIs404(t *testing.T) {
	b := newFakeBackend()
	srv := startServer(t, b, Config{})
	resp, err := http.Get(srv.URL + "/events?session=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEventsMissingSessionIs400(t *testing.T) {
	b := newFakeBackend()
	srv := startServer(t, b, Config{})
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEventsBadClaudePIDIs400(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id")
	srv := startServer(t, b, Config{})
	resp, err := http.Get(srv.URL + "/events?session=s1id&consumer=channel&claude_pid=garbage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEventsBrowserConsumerNotRegistered(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id", "feat-x")
	srv := startServer(t, b, Config{PresenceDebounce: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := connectUntilLive(t, ctx, srv.URL+"/events?session=feat-x")
	defer resp.Body.Close()

	select {
	case got := <-b.attached:
		t.Fatalf("browser connection (no consumer param) was registered as %q", got)
	default:
	}
}

func TestEventsExcludeAgentDropsAgentEcho(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id")
	b.seed("s1id",
		event.Event{Origin: event.OriginAgent, Type: otherType, Payload: []byte(`{"type":"thing.created","id":"agent"}`)},
		event.Event{Origin: event.OriginHuman, Type: otherType, Payload: []byte(`{"type":"thing.created","id":"human"}`)},
	)
	srv := startServer(t, b, Config{PresenceDebounce: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?session=s1id&exclude_origin="+event.OriginAgent+"&consumer=channel&claude_pid=7", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frames := readFramesUntilLive(t, resp.Body)
	if len(frames) != 1 || frames[0].id != 2 || !strings.Contains(frames[0].data, "human") {
		t.Fatalf("frames = %+v, want only the human event at seq 2", frames)
	}
}

func TestStaticHandlerServesAssetsAndFallsBackToIndex(t *testing.T) {
	const index = "<!doctype html><title>app</title>"
	const asset = "console.log(1)"
	dist := fstest.MapFS{
		"index.html":    {Data: []byte(index)},
		"assets/app.js": {Data: []byte(asset)},
	}
	srv := httptest.NewServer(StaticHandler(dist))
	t.Cleanup(srv.Close)

	body := func(t *testing.T, path string) string {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if got := body(t, "/assets/app.js"); got != asset {
		t.Fatalf("asset body = %q, want %q", got, asset)
	}
	if got := body(t, "/s/deep-link"); got != index {
		t.Fatalf("client-side route body = %q, want index %q", got, index)
	}
}
