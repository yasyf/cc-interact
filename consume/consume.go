// Package consume is the agent-side Server-Sent-Events client: it streams a
// subject's event log from the daemon's HTTP plane, persists a per-consumer
// cursor for at-least-once resume, and survives a daemon swap by re-resolving
// the port through a refresh handshake.
package consume

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/paths"
)

// reconnectDelay is how long a stream consumer waits before reconnecting after
// the SSE connection drops.
const reconnectDelay = 2 * time.Second

// livenessInterval is how often a pid-bound consumer checks that its owning
// window is still alive; a parked Read only wakes on the context cancel this
// triggers, never on keepalive comments.
var livenessInterval = 5 * time.Second

// EventHandler is invoked once per delivered event with its seq and raw data
// payload. Returning stop=true ends consumption (e.g. on a terminal event).
type EventHandler func(seq int64, data string) (stop bool, err error)

// StreamSource identifies one SSE consumption: where to connect, as whom, where
// to persist the cursor, and how to refresh the handshake — a version-skew
// eviction restarts the daemon mid-session, changing the port the consumer
// captured. ClaudePID 0 is a pid-less manual consumer outside any Claude window.
type StreamSource struct {
	Port      int
	SubjectID string
	Consumer  string
	ClaudePID int
	// ExcludeOrigin, when set, drops events of that origin from the stream — set it
	// to e.g. event.OriginAgent to suppress your own echo. The zero value observes
	// all origins, so a browser or parent watcher sees every origin.
	ExcludeOrigin event.Origin
	// Paths locates the per-consumer cursor file via
	// Paths.ConsumerCursorPath(SubjectID, Consumer).
	Paths paths.Paths
	// WindowAlive reports whether the owning window (ClaudePID) still lives.
	// When set alongside a non-zero ClaudePID, the consumer exits once the window
	// dies instead of leaking and draining events from a freshly-attached session;
	// nil (or a pid-less ClaudePID 0) means the consumer never self-terminates.
	WindowAlive func(pid int) bool
	Refresh     func(ctx context.Context) (port int, err error)
}

// ConsumeEvents streams a subject's events (excluding src.ExcludeOrigin, nothing
// by default) to handle, persisting a per-consumer cursor so a restart resumes
// without re-delivering. It reconnects on transient drops — refreshing the handshake
// first, so the stream survives a daemon upgrade — and returns when handle
// signals stop or ctx is cancelled. The cursor advances only after handle
// returns, so a crash mid-delivery re-delivers rather than skips (at-least-once).
func ConsumeEvents(ctx context.Context, src StreamSource, handle EventHandler) error {
	if err := src.Paths.EnsureSubjectDir(src.SubjectID); err != nil {
		return err // the cursor lives under the subject dir; without it writeCursor can't persist
	}
	cursorPath := src.Paths.ConsumerCursorPath(src.SubjectID, src.Consumer)
	cursor, err := readCursor(cursorPath)
	if err != nil {
		return err // a corrupt cursor would silently replay the whole backlog; fail loud
	}
	watched := src.ClaudePID != 0 && src.WindowAlive != nil
	for {
		if ctx.Err() != nil {
			return nil
		}
		// A consumer whose owning window is already gone exits instead of holding
		// the SSE connection (and advancing the shared cursor) forever.
		if watched && !src.WindowAlive(src.ClaudePID) {
			return nil
		}
		// Cancel the connection when the window dies mid-stream: a parked Read
		// never wakes on its own, so the liveness watchdog cancels its context.
		connCtx, cancel := context.WithCancel(ctx)
		if watched {
			go watchLiveness(connCtx, src.ClaudePID, src.WindowAlive, cancel)
		}
		stop, next, fatal := readStream(connCtx, streamURL(src), cursor, cursorPath, handle)
		cancel()
		cursor = next
		if stop || ctx.Err() != nil {
			return nil
		}
		// A liveness cancel and a transient drop both surface as fatal==nil, so
		// the re-check — not the error — decides exit vs. reconnect.
		if watched && !src.WindowAlive(src.ClaudePID) {
			return nil
		}
		// Port is the only swap signal now; on a fixed dev port a same-port daemon
		// swap is invisible, which is fine — the DB and cursors persist across it.
		refreshed := false
		if src.Refresh != nil {
			if port, err := src.Refresh(ctx); err == nil && port != 0 {
				refreshed = port != src.Port
				src.Port = port
			}
		}
		if fatal != nil && !refreshed {
			return fatal // a 4xx against the *current* handshake is permanent
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reconnectDelay):
		}
	}
}

// watchLiveness cancels the connection context once the owning window dies,
// waking a parked Read so the consumer exits. It returns when its context is
// cancelled (normal connection teardown), so it never outlives its stream.
func watchLiveness(ctx context.Context, pid int, alive func(pid int) bool, cancel context.CancelFunc) {
	t := time.NewTicker(livenessInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !alive(pid) {
				cancel()
				return
			}
		}
	}
}

func streamURL(src StreamSource) string {
	u := fmt.Sprintf("http://127.0.0.1:%d/events?session=%s&consumer=%s",
		src.Port, url.QueryEscape(src.SubjectID), url.QueryEscape(src.Consumer))
	if src.ExcludeOrigin != "" {
		u += "&exclude_origin=" + url.QueryEscape(string(src.ExcludeOrigin))
	}
	if src.ClaudePID != 0 {
		u += "&claude_pid=" + strconv.Itoa(src.ClaudePID)
	}
	return u
}

// readStream consumes one SSE connection. A nil fatal means a transient drop
// (reconnect); a non-nil fatal is a permanent failure (unknown subject, or an
// event frame larger than the buffer) the caller should surface and stop on.
func readStream(ctx context.Context, base string, cursor int64, cursorPath string, handle EventHandler) (stop bool, next int64, fatal error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return false, cursor, err
	}
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, cursor, nil // transient: connection refused / reset → reconnect
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return false, cursor, fmt.Errorf("events endpoint returned %d (unknown subject); stopping", resp.StatusCode)
		}
		return false, cursor, nil // 5xx: reconnect
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data, eventName string
	id := cursor
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// A named event (event: caught-up) is a stream-control marker, not a log
			// row: deliver only default-type frames, never on empty data.
			if data == "" || (eventName != "" && eventName != "message") {
				data, eventName = "", ""
				continue
			}
			s, herr := handle(id, data)
			if herr != nil {
				return false, cursor, nil // delivery failed: don't advance; reconnect re-delivers
			}
			cursor = id
			if err := writeCursor(cursorPath, cursor); err != nil {
				// The event was delivered (handle succeeded); we just can't persist
				// the cursor. Stop loud rather than silently replay it on the next
				// connection — a vanished subject dir is a real fault, not transient.
				return s, cursor, err
			}
			if s {
				return true, cursor, nil
			}
			data, eventName = "", ""
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		case strings.HasPrefix(line, "id:"):
			if n, e := strconv.ParseInt(strings.TrimSpace(line[len("id:"):]), 10, 64); e == nil {
				id = n
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(line[len("data:"):])
		}
	}
	if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
		return false, cursor, fmt.Errorf("event frame exceeded the %d-byte buffer; stopping: %w", 4*1024*1024, err)
	}
	return false, cursor, nil
}

// readCursor returns the persisted cursor, 0 when absent, and an error on a
// corrupt file (so a torn write replays the backlog loudly, not silently).
func readCursor(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read cursor %s: %w", path, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("corrupt cursor %s: %w", path, err)
	}
	return n, nil
}

// writeCursor persists the cursor atomically (temp + rename) so a crash can't
// leave a torn value that resets the consumer to 0. A write failure is surfaced,
// not swallowed: a silently-unpersisted cursor replays the whole backlog on the
// next connection.
func writeCursor(path string, cursor int64) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(cursor, 10)), 0o600); err != nil {
		return fmt.Errorf("write cursor %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename cursor %s: %w", path, err)
	}
	return nil
}
