package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

// DefaultConnectivityEventType is the presence event Type Connectivity emits
// when EventType is empty.
const DefaultConnectivityEventType = "channel.changed"

// Connectivity consolidates the channel-presence lifecycle. OnPresenceChange
// emits a presence event when a named consumer connects or disconnects;
// BootReconcile closes out any subject whose log still says connected, so a
// daemon death does not leave a subject wedged "connected". Wire all three into
// daemon.Config: PresenceEventType=c.Type(), OnPresenceChange=c.OnPresenceChange,
// BootReconcile=c.BootReconcile. Use c.Type() (not c.EventType) so the type sse
// filters on matches the type these methods emit even for the zero value.
//
// This generic variant emits only {"type":EventType,"connected":bool}. A
// consumer needing a richer payload — cc-review stamps each channel.changed with
// the current version_number — supplies its own OnPresenceChange and
// BootReconcile in place of these.
type Connectivity struct {
	// EventType is the presence event Type. Empty uses DefaultConnectivityEventType.
	EventType string
}

// OnPresenceChange appends a presence event for the subject via s.Append. Wire
// it to daemon.Config.OnPresenceChange.
func (c Connectivity) OnPresenceChange(ctx context.Context, s *daemon.Server, subjectID string, connected bool) {
	typ := c.Type()
	_, _ = s.Append(ctx, &event.Event{
		SubjectID: subjectID,
		Origin:    event.OriginSystem,
		Type:      typ,
		Payload:   presencePayload(typ, connected),
	})
}

// BootReconcile closes out presence state orphaned by a daemon death: for every
// subject whose latest EventType event reports connected, it appends a
// connected:false. Wire it to daemon.Config.BootReconcile, which runs it once at
// boot before the HTTP plane accepts attaches — the presence registry is empty,
// so every stale connected is provably false.
func (c Connectivity) BootReconcile(ctx context.Context, s *daemon.Server) error {
	typ := c.Type()
	ids, err := staleConnectedSubjects(ctx, s.DB(), typ)
	if err != nil {
		return fmt.Errorf("reconcile %s events: %w", typ, err)
	}
	for _, id := range ids {
		if _, err := s.Append(ctx, &event.Event{
			SubjectID: id,
			Origin:    event.OriginSystem,
			Type:      typ,
			Payload:   presencePayload(typ, false),
		}); err != nil {
			return fmt.Errorf("reconcile %s events: %w", typ, err)
		}
	}
	return nil
}

// Type is the resolved presence event Type: EventType, or
// DefaultConnectivityEventType when EventType is empty. Wire
// daemon.Config.PresenceEventType to this so sse filters the same type these
// methods emit.
func (c Connectivity) Type() string {
	if c.EventType == "" {
		return DefaultConnectivityEventType
	}
	return c.EventType
}

func presencePayload(typ string, connected bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": typ, "connected": connected})
	return b
}

// staleConnectedSubjects returns the ids of subjects whose most recent event of
// the given type reports connected:true.
func staleConnectedSubjects(ctx context.Context, db *sql.DB, eventType string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT subject_id FROM events
		WHERE type=? AND json_extract(payload,'$.connected')=1
		  AND seq=(SELECT MAX(seq) FROM events e2 WHERE e2.subject_id=events.subject_id AND e2.type=?)`,
		eventType, eventType)
	if err != nil {
		return nil, fmt.Errorf("stale connected subjects: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
