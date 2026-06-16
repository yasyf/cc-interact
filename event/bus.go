package event

import "sync"

// Bus is a per-subject publish/subscribe wakeup. A subscription is a buffered
// chan of capacity 1 carrying no payload: it is a pure edge that tells a
// consumer "something changed, re-read the event log past your cursor." Because
// every consumer always re-queries seq>cursor, a single coalesced wakeup still
// drains every event, and a wakeup delivered just before a consumer parks is
// retained by the buffer (the lost-wakeup fix).
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[*subscription]struct{}
}

type subscription struct {
	bus       *Bus
	subjectID string
	c         chan struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[string]map[*subscription]struct{}{}}
}

// Subscribe registers a subscriber for a subject and returns its wakeup channel
// plus a cancel func. Callers MUST Subscribe before their first store query and
// defer the cancel, or an event landing in the gap is lost.
func (b *Bus) Subscribe(subjectID string) (<-chan struct{}, func()) {
	sub := &subscription{bus: b, subjectID: subjectID, c: make(chan struct{}, 1)}
	b.mu.Lock()
	set := b.subs[subjectID]
	if set == nil {
		set = map[*subscription]struct{}{}
		b.subs[subjectID] = set
	}
	set[sub] = struct{}{}
	b.mu.Unlock()
	return sub.c, sub.cancel
}

func (s *subscription) cancel() {
	b := s.bus
	b.mu.Lock()
	if set := b.subs[s.subjectID]; set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(b.subs, s.subjectID)
		}
	}
	b.mu.Unlock()
}

// Publish wakes every current subscriber of a subject without ever blocking on a
// slow one: it snapshots the subscriber set under the lock, releases it (so a
// concurrent cancel can't deadlock), then does a non-blocking send into each
// cap-1 channel. A full slot is dropped because the pending wakeup already
// forces a re-query that subsumes this one.
func (b *Bus) Publish(subjectID string) {
	b.mu.Lock()
	set := b.subs[subjectID]
	subs := make([]*subscription, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		select {
		case s.c <- struct{}{}:
		default:
		}
	}
}
