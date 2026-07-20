package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/yasyf/cc-interact/event"
)

// subscriptionRegistry holds, per (subject, agent), the set of event types teed
// into that agent's mailbox. It mirrors the live agents table — evaluated when an
// agent registers, dropped when it closes, and re-derived at boot — so it carries
// no state the agents table cannot rebuild, and a restart loses nothing.
type subscriptionRegistry struct {
	mu   sync.RWMutex
	subs map[string]map[string]map[string]struct{} // subject -> agent -> event types
}

func newSubscriptionRegistry() *subscriptionRegistry {
	return &subscriptionRegistry{subs: make(map[string]map[string]map[string]struct{})}
}

// set replaces an agent's subscription with types; an empty types removes it.
func (r *subscriptionRegistry) set(subjectID, agentID string, types []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(types) == 0 {
		r.removeLocked(subjectID, agentID)
		return
	}
	byAgent := r.subs[subjectID]
	if byAgent == nil {
		byAgent = make(map[string]map[string]struct{})
		r.subs[subjectID] = byAgent
	}
	set := make(map[string]struct{}, len(types))
	for _, t := range types {
		set[t] = struct{}{}
	}
	byAgent[agentID] = set
}

// remove drops an agent's subscription.
func (r *subscriptionRegistry) remove(subjectID, agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(subjectID, agentID)
}

func (r *subscriptionRegistry) removeLocked(subjectID, agentID string) {
	byAgent := r.subs[subjectID]
	if byAgent == nil {
		return
	}
	delete(byAgent, agentID)
	if len(byAgent) == 0 {
		delete(r.subs, subjectID)
	}
}

// subscribers returns the agents on a subject subscribed to eventType.
func (r *subscriptionRegistry) subscribers(subjectID, eventType string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for agentID, set := range r.subs[subjectID] {
		if _, ok := set[eventType]; ok {
			out = append(out, agentID)
		}
	}
	return out
}

// teeToSubscribers enqueues a subject event, through Direct, into the mailbox of
// every agent subscribed to its type — as an OriginEvent directive whose text is
// the payload verbatim. Agent-origin events are never teed, breaking the handler's
// self-echo. Best-effort: the event is persisted, so a tee failure is only logged.
func (s *Server) teeToSubscribers(ctx context.Context, e *event.Event) {
	if s.subscribe == nil || e.Origin == event.OriginAgent {
		return
	}
	for _, agentID := range s.subscriptions.subscribers(e.SubjectID, e.Type) {
		if _, err := s.Direct(ctx, e.SubjectID, agentID, event.OriginEvent, string(e.Payload)); err != nil {
			s.log.Printf("tee %s to %s/%s: %v", e.Type, e.SubjectID, agentID, err)
		}
	}
}

// reconcileSubscriptions re-derives every live agent's subscription from the
// persisted agents table, so a daemon restart rebuilds the in-memory registry
// without a table of its own.
func (s *Server) reconcileSubscriptions(ctx context.Context) error {
	if s.subscribe == nil {
		return nil
	}
	agents, err := s.store.ListRunningAgents(ctx)
	if err != nil {
		return fmt.Errorf("reconcile subscriptions: %w", err)
	}
	for _, info := range agents {
		sub, err := s.subjects.Store.Get(ctx, info.SubjectID)
		if err != nil {
			return fmt.Errorf("reconcile subscriptions %s: %w", info.SubjectID, err)
		}
		s.subscriptions.set(info.SubjectID, info.AgentID, s.subscribe(sub, info))
	}
	return nil
}

// muteFrame reports whether the event is withheld from consumer: only the
// configured mute consumer is affected, and only for a type a currently-present
// subscriber owns. Presence lapsing (a dead handler) resumes delivery.
func (s *Server) muteFrame(subjectID, consumer string, e event.Event) bool {
	if consumer != s.muteConsumer {
		return false
	}
	for _, agentID := range s.subscriptions.subscribers(subjectID, e.Type) {
		if s.parks.present(subjectID, agentID, subscriberPresenceWindow) {
			return true
		}
	}
	return false
}
