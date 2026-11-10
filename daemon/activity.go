package daemon

import (
	"strconv"
	"sync"
	"time"
)

// Activity tracks which stream consumers are wired to a subject: live SSE
// attachments (per subject, keyed by consumer name + window pid), anonymous
// browser streams (per subject), and recent resolve polls (per scope, same key). It is how a domain handler can report a
// consumer's presence without blocking on one (e.g. a status connected readout).
type Activity struct {
	mu       sync.Mutex
	attached map[string]map[attachKey]int
	streams  map[string]map[attachKey]map[*pushSpan]struct{}
	viewers  map[string]int
	lastDrop map[string]time.Time
	polls    map[string]time.Time
	now      func() time.Time
}

type attachKey struct {
	consumer string
	pid      int
}

type pushSpan struct {
	after, through int64
}

// NewActivity returns an empty registry.
func NewActivity() *Activity {
	return &Activity{
		attached: make(map[string]map[attachKey]int),
		streams:  make(map[string]map[attachKey]map[*pushSpan]struct{}),
		viewers:  make(map[string]int),
		lastDrop: make(map[string]time.Time),
		polls:    make(map[string]time.Time),
		now:      time.Now,
	}
}

// Attach records one open SSE connection for a consumer in a window, resuming
// after cursor, and returns its pushed, which records that the connection has
// written every frame through a seq, and its detach. Counting (not a flag) keeps
// an overlapping reconnect attached; the subject's last detach stamps lastDrop
// for AttachedWithin.
func (a *Activity) Attach(subjectID, consumer string, pid int, cursor int64) (pushed func(seq int64), detach func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.attached[subjectID]
	if m == nil {
		m = make(map[attachKey]int)
		a.attached[subjectID] = m
	}
	k := attachKey{consumer, pid}
	m[k]++
	if a.streams[subjectID] == nil {
		a.streams[subjectID] = make(map[attachKey]map[*pushSpan]struct{})
	}
	if a.streams[subjectID][k] == nil {
		a.streams[subjectID][k] = make(map[*pushSpan]struct{})
	}
	span := &pushSpan{after: cursor, through: cursor}
	a.streams[subjectID][k][span] = struct{}{}
	var once sync.Once
	return func(seq int64) {
			a.mu.Lock()
			defer a.mu.Unlock()
			span.through = max(span.through, seq)
		}, func() {
			once.Do(func() {
				a.mu.Lock()
				defer a.mu.Unlock()
				delete(a.streams[subjectID][k], span)
				if len(a.streams[subjectID][k]) == 0 {
					delete(a.streams[subjectID], k)
				}
				if m[k]--; m[k] <= 0 {
					delete(m, k)
				}
				if len(m) == 0 {
					a.lastDrop[subjectID] = a.now()
				}
			})
		}
}

// AttachViewer records one open anonymous (browser) SSE stream for a subject and
// returns its detach.
func (a *Activity) AttachViewer(subjectID string) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.viewers[subjectID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.viewers[subjectID]--; a.viewers[subjectID] <= 0 {
				delete(a.viewers, subjectID)
			}
		})
	}
}

// Viewing reports whether any anonymous (browser) SSE stream is open on the
// subject.
func (a *Activity) Viewing(subjectID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.viewers[subjectID] > 0
}

// Attached reports whether the consumer in that window has an open SSE
// connection to the subject.
func (a *Activity) Attached(subjectID, consumer string, pid int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attached[subjectID][attachKey{consumer, pid}] > 0
}

// Pushed reports the span of seqs the consumer's one open connection in that
// window has written since it resumed: past its resume cursor, through the last
// seq it flushed. ok is false unless that connection is the consumer's only one
// on the subject, in any window: overlapping connections leave no single span
// that every one of them pushed, and they share one persisted cursor.
func (a *Activity) Pushed(subjectID, consumer string, pid int) (after, through int64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var only *pushSpan
	for key, spans := range a.streams[subjectID] {
		if key.consumer != consumer {
			continue
		}
		for span := range spans {
			if only != nil || key.pid != pid {
				return 0, 0, false
			}
			only = span
		}
	}
	if only == nil {
		return 0, 0, false
	}
	return only.after, only.through, true
}

// Counts reports live SSE connection counts by consumer name for subjectID,
// summing connections across PIDs.
func (a *Activity) Counts(subjectID string) map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	counts := make(map[string]int)
	for key, count := range a.attached[subjectID] {
		counts[key.consumer] += count
	}
	return counts
}

// AttachedWithin reports whether any consumer is attached to the subject now, or
// the subject's last attachment dropped within grace of now.
func (a *Activity) AttachedWithin(subjectID string, grace time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.attached[subjectID]) > 0 {
		return true
	}
	t, ok := a.lastDrop[subjectID]
	return ok && a.now().Sub(t) <= grace
}

// NotePoll records that the consumer in that window just polled resolve for this
// scope.
func (a *Activity) NotePoll(scope, consumer string, pid int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.polls[pollKey(scope, consumer, pid)] = a.now()
}

// ResolvePollCeiling is the longest a channel waiting for its subject goes
// between resolve polls, so a PolledSince window must exceed it.
const ResolvePollCeiling = 5 * time.Second

// PolledSince reports whether the consumer in that window polled for this scope
// within window.
func (a *Activity) PolledSince(scope, consumer string, pid int, window time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.polls[pollKey(scope, consumer, pid)]
	return ok && a.now().Sub(t) <= window
}

func pollKey(scope, consumer string, pid int) string {
	return scope + "\x00" + consumer + "\x00" + strconv.Itoa(pid)
}
