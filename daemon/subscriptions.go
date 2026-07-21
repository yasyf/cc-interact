package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/agent"
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

// supersededDirective is the terminal directive text delivered to a superseded
// subscriber; its event.OriginSupersede origin marks it a supersede, not guidance.
const supersededDirective = "A newer handler superseded this one on the subject; stop and exit."

// supersedeSubscribers closes every other running same-type agent on keep's
// subject. Candidates are authoritative: every StatusRunning same-(subject,
// agent_type) row, registry membership irrelevant, so a zombie or reconcile
// loser absent from the registry is swept too. Each loser leaves the registry,
// gets a terminal OriginSupersede directive that wakes its park, and is closed.
//
// Supersede does not migrate the loser's undrained mailbox; consumers reconcile
// from authoritative state on start.
func (s *Server) supersedeSubscribers(ctx context.Context, subjectID string, keep agent.Info) error {
	others, err := s.store.ListRunningAgentsByType(ctx, subjectID, keep.AgentType)
	if err != nil {
		return fmt.Errorf("supersede %s: %w", subjectID, err)
	}
	for _, other := range others {
		if other.AgentID == keep.AgentID {
			continue
		}
		s.subscriptions.remove(subjectID, other.AgentID)
		if _, err := s.Direct(ctx, subjectID, other.AgentID, event.OriginSupersede, supersededDirective); err != nil {
			return fmt.Errorf("supersede %s/%s: %w", subjectID, other.AgentID, err)
		}
		closed, err := s.store.CloseAgent(ctx, subjectID, other.AgentID, time.Now())
		if err != nil {
			return fmt.Errorf("supersede %s/%s: %w", subjectID, other.AgentID, err)
		}
		if closed {
			if err := s.recordAgentStopped(ctx, subjectID, other.AgentID, stoppedEvent(subjectID, agentStopBody{AgentID: other.AgentID})); err != nil {
				return fmt.Errorf("supersede %s/%s: %w", subjectID, other.AgentID, err)
			}
		}
	}
	return nil
}

// singletonKey groups running subscribers by subject and agent type for the
// singleton-subscriber rebuild.
type singletonKey struct {
	subjectID string
	agentType string
}

// reconcileSubscriptions re-derives every live agent's subscription from the
// persisted agents table, rebuilding the in-memory registry after a restart. The
// invariant, at most one live agent per subscribed (subject, agent_type), is
// enforced at each registration; this rebuild only prunes the registry to each
// group's last-registered winner, leaving stale rows for the next one to sweep.
//
// Insertion order (rowid, the ListRunningAgents order) defines recency.
func (s *Server) reconcileSubscriptions(ctx context.Context) error {
	if s.subscribe == nil {
		return nil
	}
	agents, err := s.store.ListRunningAgents(ctx)
	if err != nil {
		return fmt.Errorf("reconcile subscriptions: %w", err)
	}
	winners := make(map[singletonKey]agent.Info)
	for _, info := range agents {
		sub, err := s.subjects.Store.Get(ctx, info.SubjectID)
		if err != nil {
			return fmt.Errorf("reconcile subscriptions %s: %w", info.SubjectID, err)
		}
		types := s.subscribe(sub, info)
		s.subscriptions.set(info.SubjectID, info.AgentID, types)
		if !s.singletonSubscriber || len(types) == 0 {
			continue
		}
		k := singletonKey{info.SubjectID, info.AgentType}
		if held, ok := winners[k]; ok {
			s.subscriptions.remove(held.SubjectID, held.AgentID)
		}
		winners[k] = info
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
