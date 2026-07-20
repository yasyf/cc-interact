package daemon

import (
	"sync"
	"time"
)

// parkRegistry tracks agent await-presence: the release channel of the one await
// long-poll parked on each (subject, agent), plus when that agent last drained.
// In-memory and dropped on restart by design — the child's next await re-drains
// the persisted pending rows, so nothing is lost.
type parkRegistry struct {
	mu      sync.Mutex
	waiters map[parkKey]chan struct{}
	drains  map[parkKey]time.Time
	now     func() time.Time
}

type parkKey struct {
	subjectID string
	agentID   string
}

// newParkRegistry returns an empty registry.
func newParkRegistry() *parkRegistry {
	return &parkRegistry{
		waiters: make(map[parkKey]chan struct{}),
		drains:  make(map[parkKey]time.Time),
		now:     time.Now,
	}
}

// wait registers a park for (subjectID, agentID) and returns its release channel,
// buffered so release never blocks on a waiter that has not yet parked. One waiter
// per key: an existing waiter is signalled as it is displaced, so the orphaned
// await wakes and re-drains instead of stalling to its timeout.
func (p *parkRegistry) wait(subjectID, agentID string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := parkKey{subjectID, agentID}
	if old, ok := p.waiters[k]; ok {
		select {
		case old <- struct{}{}:
		default:
		}
	}
	ch := make(chan struct{}, 1)
	p.waiters[k] = ch
	return ch
}

// release wakes the park for (subjectID, agentID) when one is registered.
func (p *parkRegistry) release(subjectID, agentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.waiters[parkKey{subjectID, agentID}]
	if !ok {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// done deregisters the park iff ch is still the registered waiter, leaving a
// newer park for the same key intact.
func (p *parkRegistry) done(subjectID, agentID string, ch chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := parkKey{subjectID, agentID}
	if p.waiters[k] == ch {
		delete(p.waiters, k)
	}
}

// waiting reports whether a park is registered for (subjectID, agentID).
func (p *parkRegistry) waiting(subjectID, agentID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.waiters[parkKey{subjectID, agentID}]
	return ok
}

// noteDrain records that the agent just drained its mailbox, extending its
// presence across the gap between a delivering await and the next park.
func (p *parkRegistry) noteDrain(subjectID, agentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drains[parkKey{subjectID, agentID}] = p.now()
}

// present reports whether the agent holds live presence: a currently-parked await
// or a mailbox drain within window. A kill-9'd child holds neither once window
// lapses, so presence never wedges on registration alone.
func (p *parkRegistry) present(subjectID, agentID string, window time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := parkKey{subjectID, agentID}
	if _, ok := p.waiters[k]; ok {
		return true
	}
	last, ok := p.drains[k]
	return ok && p.now().Sub(last) <= window
}
