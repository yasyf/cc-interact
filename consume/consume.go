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
	// Paths locates the per-consumer cursor file via
	// Paths.ConsumerCursorPath(SubjectID, Consumer).
	Paths   paths.Paths
	Refresh func(ctx context.Context) (port int, err error)
}

// ConsumeEvents streams a subject's events (excluding the agent's own) to
// handle, persisting a per-consumer cursor so a restart resumes without
// re-delivering. It reconnects on transient drops — refreshing the handshake
// first, so the stream survives a daemon upgrade — and returns when handle
// signals stop or ctx is cancelled. The cursor advances only after handle
// returns, so a crash mid-delivery re-delivers rather than skips (at-least-once).
func ConsumeEvents(ctx context.Context, src StreamSource, handle EventHandler) error {
	cursorPath := src.Paths.ConsumerCursorPath(src.SubjectID, src.Consumer)
	cursor, err := readCursor(cursorPath)
	if err != nil {
		return err // a corrupt cursor would silently replay the whole backlog; fail loud
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		stop, next, fatal := readStream(ctx, streamURL(src), cursor, cursorPath, handle)
		cursor = next
		if stop || ctx.Err() != nil {
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

func streamURL(src StreamSource) string {
	u := fmt.Sprintf("http://127.0.0.1:%d/events?session=%s&exclude_origin=%s&consumer=%s",
		src.Port, url.QueryEscape(src.SubjectID), event.OriginAgent, url.QueryEscape(src.Consumer))
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
	var data string
	id := cursor
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data == "" {
				continue
			}
			s, herr := handle(id, data)
			if herr != nil {
				return false, cursor, nil // delivery failed: don't advance; reconnect re-delivers
			}
			cursor = id
			writeCursor(cursorPath, cursor)
			if s {
				return true, cursor, nil
			}
			data = ""
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		case strings.HasPrefix(line, "id:"):
			if n, e := strconv.ParseInt(strings.TrimSpace(line[len("id:"):]), 10, 64); e == nil {
				id = n
			}
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
// leave a torn value that resets the consumer to 0.
func writeCursor(path string, cursor int64) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(cursor, 10)), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
