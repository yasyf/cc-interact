// Package consume is the agent-side Server-Sent-Events client: it streams a
// subject's event log from the daemon's HTTP plane, persists a per-consumer
// cursor for at-least-once resume, warns without stopping when persistence
// fails, and survives a daemon swap by re-resolving the port through a refresh
// handshake.
package consume

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/internal/statepath"
	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/paths"
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
	// Paths locates cc-interact's exact v1 cursor namespace.
	Paths paths.Paths
	// Warn is invoked when persisting the resume cursor fails; the zero value
	// logs to stderr.
	Warn func(error)
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
// Cursor persistence failures invoke src.Warn and streaming continues; a later
// restart may therefore re-deliver already handled events.
func ConsumeEvents(ctx context.Context, src StreamSource, handle EventHandler) error {
	if err := statepath.EnsureSubjectDir(src.Paths, src.SubjectID); err != nil {
		return fmt.Errorf("ensure cursor directory for %s: %w", src.SubjectID, err)
	}
	cursorPath := statepath.Cursor(src.Paths, src.SubjectID, src.Consumer)
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
		stop, next, fatal := readStream(connCtx, streamURL(src), cursor, cursorPath, src.Warn, handle)
		cancel()
		cursor = next
		if stop || ctx.Err() != nil {
			return fatal
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
func readStream(ctx context.Context, base string, cursor int64, cursorPath string, warn func(error), handle EventHandler) (stop bool, next int64, fatal error) {
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
				warnError(warn, err)
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

// SeedCursor seeds self from the furthest process-owned v1 cursor for base,
// then removes cursor files belonging to dead sibling processes.
func SeedCursor(p paths.Paths, subjectID, base, self string) error {
	return seedCursor(p, subjectID, base, self, nil)
}

func seedCursor(p paths.Paths, subjectID, base, self string, warn func(error)) error {
	if err := statepath.EnsureSubjectDir(p, subjectID); err != nil {
		return fmt.Errorf("ensure cursor directory for %s: %w", subjectID, err)
	}
	selfPath := statepath.Cursor(p, subjectID, self)
	seed, err := readCursor(selfPath)
	if err != nil {
		return err
	}

	subjectDir := statepath.SubjectDir(p, subjectID)
	entries, err := os.ReadDir(subjectDir)
	if err != nil {
		return fmt.Errorf("scan cursor directory %s: %w", subjectDir, err)
	}
	prefix, suffix := base+"-", ".cursor"
	type sibling struct {
		path string
		pid  int32
	}
	var siblings []sibling
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		pid, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix), 10, 32)
		if err != nil || pid <= 0 {
			continue
		}
		path := filepath.Join(subjectDir, name)
		siblings = append(siblings, sibling{path: path, pid: int32(pid)})
		if path == selfPath {
			continue
		}
		cursor, err := readCursor(path)
		if err != nil {
			var parseErr *strconv.NumError
			if errors.As(err, &parseErr) {
				warnError(warn, err)
				continue
			}
			return err
		}
		if cursor > seed {
			seed = cursor
		}
	}

	for _, sibling := range siblings {
		exists, err := process.PidExists(sibling.pid)
		if err != nil {
			return fmt.Errorf("check cursor process %d: %w", sibling.pid, err)
		}
		if exists {
			continue
		}
		if err := os.Remove(sibling.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove dead cursor %s: %w", sibling.path, err)
		}
	}
	if seed == 0 {
		return nil
	}
	return writeCursor(selfPath, seed)
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

// writeCursor persists the cursor atomically and durably through a uniquely
// named temporary file so concurrent writers never share rename state.
func writeCursor(path string, cursor int64) error {
	if err := dkdaemon.WriteFileDurable(path, []byte(strconv.FormatInt(cursor, 10)), 0o600); err != nil {
		return fmt.Errorf("persist cursor %s: %w", path, err)
	}
	return nil
}

func warnError(warn func(error), err error) {
	if warn != nil {
		warn(err)
		return
	}
	fmt.Fprintf(os.Stderr, "cc-interact: %v\n", err)
}
