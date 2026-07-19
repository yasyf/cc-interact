package daemon

import "sync"

// parkRegistry maps a (subject, agent) to the release channel of the one await
// long-poll parked on it, mirroring Activity: an in-memory structure the daemon
// drops on restart by design. Direct signals a park; the parked await re-drains
// on release. A restart loses every park — the child's next await drains the
// persisted pending rows first, so nothing is lost.
type parkRegistry struct {
	mu      sync.Mutex
	waiters map[parkKey]chan struct{}
}

type parkKey struct {
	subjectID string
	agentID   string
}

// newParkRegistry returns an empty registry.
func newParkRegistry() *parkRegistry {
	return &parkRegistry{waiters: make(map[parkKey]chan struct{})}
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
