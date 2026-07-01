// Package sse is cc-interact's realtime event plane: a 127.0.0.1 HTTP server
// that fans a subject's append-only event log out to every consumer over
// Server-Sent Events. The agent's own stream consumers (its Monitor, the MCP
// channel) read this same GET /events endpoint, so the plane is core, not
// optional. The package owns the mux and always mounts GET /events; the consumer
// mounts its own routes — a REST surface, the opt-in StaticHandler — onto the
// exposed Mux. It depends only on the event package, so there is no import cycle
// with the daemon that implements Backend.
package sse

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/event"
)

const keepaliveInterval = 20 * time.Second

// DefaultPresenceDebounce is how long after a named consumer's last detach the
// handler waits before re-reading ConsumerConnected and emitting a
// connected=false presence change. It must outlast the backend's attach grace
// plus the consumers' reconnect delay, so a transient drop never persists a
// disconnected event. Override per-server via Config.PresenceDebounce.
const DefaultPresenceDebounce = 15 * time.Second

// Backend is the daemon-side capability the SSE plane needs: resolve a subject
// ref to its canonical id, read the event log past a cursor, subscribe to a
// subject's wakeup bus, and track named-consumer presence. A daemon satisfies
// this by delegating to its store, its event.Bus, and its presence registry.
type Backend interface {
	// ResolveSubject maps a ref — a slug (what a browser sends) or a canonical id
	// (what an agent-side consumer sends) — to the canonical subject id that keys
	// the Bus and the events table. found is false for an unknown ref.
	ResolveSubject(ctx context.Context, ref string) (subjectID string, found bool, err error)

	// EventsSince returns events with seq greater than cursor, oldest first.
	// excludeOrigin drops rows of that origin so a consumer can suppress its own
	// echo; "" (what a browser sends) returns every origin.
	EventsSince(ctx context.Context, subjectID string, cursor int64, excludeOrigin string) ([]event.Event, error)

	// Subscribe registers a wakeup subscriber for a subject and returns its signal
	// channel plus a cancel func. The handler Subscribes before its first
	// EventsSince so an event landing in the gap is not lost.
	Subscribe(subjectID string) (<-chan struct{}, func())

	// Attach records one open named-consumer SSE connection (consumer name +
	// window pid) and returns its detach. It always runs for a named consumer,
	// independent of presence emission, so the registry feeding ConsumerConnected
	// stays accurate.
	Attach(subjectID, consumer string, pid int) func()

	// ConsumerConnected reports whether a live named consumer is currently wired
	// to the subject. It gates the presence-change transitions.
	ConsumerConnected(subjectID string) bool
}

// Config tunes the server. The zero value is valid: no presence emission, the
// default debounce, every frame delivered to named consumers.
type Config struct {
	// OnPresenceChange fires when a named consumer's connectivity to a subject
	// transitions: connected=true on the first attach, connected=false after the
	// debounce following the last detach if still disconnected. The consumer wires
	// this to emit its own presence event (cc-review: channel.changed). nil
	// disables presence emission; Attach still runs.
	OnPresenceChange func(ctx context.Context, subjectID string, connected bool)

	// PresenceEventType is the event Type that OnPresenceChange emits. Frames of
	// this type are dropped from named consumers — the connectivity flip a named
	// consumer caused must not wake it — while the cursor still advances past the
	// skipped row so a reconnect never re-queries the filtered tail. Empty
	// delivers every frame to named consumers.
	PresenceEventType string

	// PresenceDebounce overrides DefaultPresenceDebounce when non-zero.
	PresenceDebounce time.Duration
}

// Server is the HTTP handler tree. It owns the mux and always mounts GET
// /events; the consumer mounts its own routes via Mux. The listener (the
// consumer's responsibility) binds 127.0.0.1 only, which is the whole
// access-control story.
type Server struct {
	backend  Backend
	cfg      Config
	mux      *http.ServeMux
	debounce time.Duration

	injectMu sync.Mutex
	injects  map[injectKey]map[chan string]struct{}
}

// NewServer builds the server and mounts GET /events. Register additional routes
// on Mux before serving.
func NewServer(backend Backend, cfg Config) *Server {
	debounce := cfg.PresenceDebounce
	if debounce == 0 {
		debounce = DefaultPresenceDebounce
	}
	s := &Server{
		backend: backend, cfg: cfg, mux: http.NewServeMux(), debounce: debounce,
		injects: make(map[injectKey]map[chan string]struct{}),
	}
	s.mux.HandleFunc("GET /events", s.handleEvents)
	return s
}

// Mux returns the server's mux so the consumer can register its own routes — a
// REST surface, the opt-in StaticHandler on the catch-all "/". Go's pattern mux
// resolves by specificity, not registration order, so routes added after
// construction never shadow GET /events.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Handler returns the root handler to serve.
func (s *Server) Handler() http.Handler { return s.mux }

// handleEvents streams a subject's event log as Server-Sent Events. ?session= is
// a subject ref — a browser sends the slug, an agent-side consumer sends the
// canonical id — resolved here to the id that keys the Bus and the events table.
// A browser omits exclude_origin and sees every origin; a consumer passes
// exclude_origin=<origin> to drop that origin (its own echo). Resume is via Last-Event-ID
// (header, or the ?last_event_id= query fallback for native EventSource, which
// cannot set headers on the initial request).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("session")
	if ref == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	subjectID, found, err := s.backend.ResolveSubject(r.Context(), ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	excludeOrigin := r.URL.Query().Get("exclude_origin")
	// Named stream consumers (the agent's Monitor, the MCP channel) register their
	// presence with their window pid; a browser sends neither param and is never
	// registered. An absent claude_pid is a pid-less manual consumer (0), not an
	// error; garbage is.
	consumer := r.URL.Query().Get("consumer")
	pid := 0
	if v := r.URL.Query().Get("claude_pid"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "bad claude_pid", http.StatusBadRequest)
			return
		}
		pid = n
	}
	// A named consumer's attach/detach drives the presence transition: a first
	// attach emits connected, and a detach that outlives the debounce (no consumer
	// came back) emits disconnected. The consumer persists the result last-wins.
	// Two near-zero-width races are accepted: concurrent first attaches can
	// double-emit connected:true (idempotent), and an attach landing between the
	// debounce predicate check and the false emit briefly inverts to false until
	// the next transition. A daemon death loses the detach defer and the debounce
	// timer; the consumer reconciles the stale connected:true at its next boot.
	// Attach always runs so the registry feeding ConsumerConnected stays accurate;
	// only the emit is gated on OnPresenceChange.
	var inject chan string
	if consumer != "" {
		wasConnected := s.backend.ConsumerConnected(subjectID)
		detach := s.backend.Attach(subjectID, consumer, pid)
		if !wasConnected {
			s.presenceChange(r.Context(), subjectID, true)
		}
		defer func() {
			detach()
			time.AfterFunc(s.debounce, func() {
				if !s.backend.ConsumerConnected(subjectID) {
					s.presenceChange(context.Background(), subjectID, false)
				}
			})
		}()
		// Only a named consumer can receive solicited frames (Inject); a browser's
		// inject channel stays nil, which never fires in the select.
		inject = s.attachInject(injectKey{subjectID, consumer, pid})
		defer s.detachInject(injectKey{subjectID, consumer, pid}, inject)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	cursor := parseCursor(r)

	// Subscribe BEFORE the first query so an event committed during replay is not
	// lost between the gap query and the park (the cap-1 buffer retains the edge).
	signal, unsub := s.backend.Subscribe(subjectID)
	defer unsub()

	ctx := r.Context()
	cursor = s.flushSince(ctx, w, flusher, subjectID, cursor, excludeOrigin, consumer != "")
	io.WriteString(w, ": connected\n\n") // prove liveness + flush proxies
	flusher.Flush()

	ka := time.NewTicker(keepaliveInterval)
	defer ka.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ka.C:
			io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case <-signal:
			cursor = s.flushSince(ctx, w, flusher, subjectID, cursor, excludeOrigin, consumer != "")
		case p := <-inject:
			// No id: the frame is outside the log, so the consumer's cursor must not
			// advance past real events it has not seen.
			fmt.Fprintf(w, "data: %s\n\n", p)
			flusher.Flush()
		}
	}
}

// flushSince writes one SSE frame per event with seq greater than cursor and
// returns the new high-water cursor. For a named consumer it drops frames of the
// configured presence type — the connectivity flip a named consumer caused must
// not wake it — but the cursor advances past skipped rows too, so a wake never
// re-queries the filtered tail. One query per wake; no DB handle is held across
// the select.
func (s *Server) flushSince(ctx context.Context, w io.Writer, fl http.Flusher, subjectID string, cursor int64, excludeOrigin string, named bool) int64 {
	evs, err := s.backend.EventsSince(ctx, subjectID, cursor, excludeOrigin)
	if err != nil {
		return cursor
	}
	wrote := false
	for _, e := range evs {
		if e.Seq > cursor {
			cursor = e.Seq
		}
		if named && s.cfg.PresenceEventType != "" && e.Type == s.cfg.PresenceEventType {
			continue
		}
		// No `event:` field: native EventSource delivers only default-type frames
		// to onmessage, which is how a browser consumes the stream. The frame's
		// type lives inside the JSON payload instead.
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, e.Payload)
		wrote = true
	}
	if wrote {
		fl.Flush()
	}
	return cursor
}

// injectKey addresses one window's named stream: Inject targets exactly the
// (subject, consumer, pid) triple so a solicited frame can never wake another
// window's consumer.
type injectKey struct {
	subjectID string
	consumer  string
	pid       int
}

// Inject writes a one-shot frame to every live stream matching the key and
// reports how many received it. The frame bypasses the event log and carries no
// id, so the consumer's cursor never advances and a reconnect can never replay
// it — the shape for solicited signals (a delivery probe) that must arrive once
// or not at all.
func (s *Server) Inject(subjectID, consumer string, pid int, payload string) int {
	s.injectMu.Lock()
	defer s.injectMu.Unlock()
	n := 0
	for ch := range s.injects[injectKey{subjectID, consumer, pid}] {
		select {
		case ch <- payload:
			n++
		default: // a stream that stopped draining is not worth parking the caller on
		}
	}
	return n
}

func (s *Server) attachInject(k injectKey) chan string {
	ch := make(chan string, 1)
	s.injectMu.Lock()
	defer s.injectMu.Unlock()
	if s.injects[k] == nil {
		s.injects[k] = make(map[chan string]struct{})
	}
	s.injects[k][ch] = struct{}{}
	return ch
}

func (s *Server) detachInject(k injectKey, ch chan string) {
	s.injectMu.Lock()
	defer s.injectMu.Unlock()
	delete(s.injects[k], ch)
	if len(s.injects[k]) == 0 {
		delete(s.injects, k)
	}
}

func (s *Server) presenceChange(ctx context.Context, subjectID string, connected bool) {
	if s.cfg.OnPresenceChange != nil {
		s.cfg.OnPresenceChange(ctx, subjectID, connected)
	}
}

func parseCursor(r *http.Request) int64 {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	if v := r.URL.Query().Get("last_event_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}
