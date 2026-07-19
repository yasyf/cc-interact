package cmd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yasyf/cc-interact/daemon"
)

// resolveSubject polls the daemon until a subject exists for the session+scope,
// returning its id and the HTTP handshake port. A stream consumer may start
// before the subject is created; consumer names the caller so the daemon tracks
// its presence. A control-socket error stops the poll (the daemon is up by the
// time watch resolves).
func resolveSubject(ctx context.Context, client *daemon.Client, session, scope string, claudePID int, consumer string) (subjectID string, port int, err error) {
	for {
		reply, err := client.Do(ctx, daemon.Envelope{
			Op: daemon.OpResolve, Session: session, ClaudePID: claudePID, Scope: scope, Consumer: consumer,
		})
		if err != nil {
			return "", 0, err
		}
		if reply.SubjectID != "" {
			return reply.SubjectID, reply.HTTPPort, nil
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// waitForSubject polls until a subject exists, tolerating a missing daemon — the
// channel server loads at session start, before either the daemon or the subject
// is guaranteed up, and lives for the whole window. It returns ("", 0) only when
// ctx is cancelled.
func waitForSubject(ctx context.Context, connect func(context.Context) (*daemon.Client, error), session, scope string, claudePID int, consumer string) (subjectID string, port int) {
	for {
		if ctx.Err() != nil {
			return "", 0
		}
		client, err := connect(ctx)
		if err == nil {
			reply, err := client.Do(ctx, daemon.Envelope{
				Op: daemon.OpResolve, Session: session, ClaudePID: claudePID, Scope: scope, Consumer: consumer,
			})
			_ = client.Close()
			if err == nil && reply.SubjectID != "" {
				return reply.SubjectID, reply.HTTPPort
			}
		}
		select {
		case <-ctx.Done():
			return "", 0
		case <-time.After(time.Second):
		}
	}
}

// refreshHandshake returns a consume.StreamSource.Refresh that re-resolves the
// daemon's current HTTP port, so a stream survives an exact-build daemon
// replacement.
func refreshHandshake(connect func(context.Context) (*daemon.Client, error), session, scope string, claudePID int, consumer string) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		client, err := connect(ctx)
		if err != nil {
			return 0, err
		}
		defer func() { _ = client.Close() }()
		reply, err := client.Do(ctx, daemon.Envelope{
			Op: daemon.OpResolve, Session: session, ClaudePID: claudePID, Scope: scope, Consumer: consumer,
		})
		if err != nil {
			return 0, err
		}
		return reply.HTTPPort, nil
	}
}

// eventType extracts the `type` field from an event's JSON payload.
func eventType(data string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	return e.Type
}
