package daemon

import (
	"strconv"
	"sync"
	"time"
)

// Activity tracks which stream consumers are wired to a subject: live SSE
// attachments (per subject, keyed by consumer name + window pid) and recent
// resolve polls (per scope, same key). It is how a domain handler can report a
// consumer's presence without blocking on one (e.g. a status connected readout).
// proven records windows whose model acked a delivered channel tag; proof lasts
// the daemon's lifetime — pid-recycle inheritance is accepted because active
// presence also requires a live attachment.
type Activity struct {
	mu       sync.Mutex
	attached map[string]map[attachKey]int
	lastDrop map[string]time.Time
	polls    map[string]time.Time
	proven   map[int]struct{}
	now      func() time.Time
}

type attachKey struct {
	consumer string
	pid      int
}

// NewActivity returns an empty registry.
func NewActivity() *Activity {
	return &Activity{
		attached: make(map[string]map[attachKey]int),
		lastDrop: make(map[string]time.Time),
		polls:    make(map[string]time.Time),
		proven:   make(map[int]struct{}),
		now:      time.Now,
	}
}

// Attach records one open SSE connection for a consumer in a window and returns
// its detach. Counting (not a flag) keeps an overlapping reconnect attached; the
// subject's last detach stamps lastDrop for AttachedWithin.
func (a *Activity) Attach(subjectID, consumer string, pid int) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.attached[subjectID]
	if m == nil {
		m = make(map[attachKey]int)
		a.attached[subjectID] = m
	}
	k := attachKey{consumer, pid}
	m[k]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if m[k]--; m[k] <= 0 {
				delete(m, k)
			}
			if len(m) == 0 {
				a.lastDrop[subjectID] = a.now()
			}
		})
	}
}

// Attached reports whether the consumer in that window has an open SSE
// connection to the subject.
func (a *Activity) Attached(subjectID, consumer string, pid int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attached[subjectID][attachKey{consumer, pid}] > 0
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

// PolledSince reports whether the consumer in that window polled for this scope
// within window.
func (a *Activity) PolledSince(scope, consumer string, pid int, window time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.polls[pollKey(scope, consumer, pid)]
	return ok && a.now().Sub(t) <= window
}

// MarkProven records that the window's model acked a delivered channel tag.
func (a *Activity) MarkProven(pid int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proven[pid] = struct{}{}
}

// Proven reports whether the window's channel round trip has been proven.
func (a *Activity) Proven(pid int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.proven[pid]
	return ok
}

func pollKey(scope, consumer string, pid int) string {
	return scope + "\x00" + consumer + "\x00" + strconv.Itoa(pid)
}
