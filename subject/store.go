package subject

import "context"

// Store is the persistence the Resolver drives. The SQL implementation lives in
// a separate package; this is only the contract the resolver depends on.
type Store interface {
	// FindBySessionScope returns the subject bound to an exact (session, scope)
	// pair. ok is false when none exists; a blank session never matches.
	FindBySessionScope(ctx context.Context, session, scope string) (Subject, bool, error)

	// FindLatestByWindowScope returns the most recent subject owned by a window
	// (claudePID) in a scope, in ANY status — the caller filters via
	// Policy.Active. ok is false when none exists; claudePID 0 (detached) never
	// matches.
	FindLatestByWindowScope(ctx context.Context, claudePID int, scope string) (Subject, bool, error)

	// FindAdoptableByScope returns the most recent adoptable subject for a scope
	// regardless of session id — those whose status is in the store's active set
	// (cc-review: status='open'), most recent first. ok is false when none
	// exists. The resolver additionally gates the result through Policy.Active
	// and Policy.Held before adopting.
	FindAdoptableByScope(ctx context.Context, scope string) (Subject, bool, error)

	// Create inserts a new subject owned by the window and returns it with its
	// timestamps populated. A blank session is stored as NULL so the unique
	// (session, scope) slot is not collapsed across session-less subjects.
	Create(ctx context.Context, id, slug, session, scope string, claudePID int, status string) (Subject, error)

	// Rebind compare-and-swaps a subject's owning window: it sets session and
	// claudePID only if the subject's claudePID still equals fromPID, returning
	// whether exactly one row changed
	// (UPDATE ... SET session_id=?, claude_pid=?, updated_at=? WHERE id=? AND claude_pid=?).
	// There is no status gate — resuming a non-active subject across session
	// rotation is legitimate. A unique-index violation (session already owns
	// another subject in the scope) propagates as the error.
	Rebind(ctx context.Context, id string, fromPID int, session string, claudePID int) (bool, error)

	// SetStatus updates a subject's status and bumps updated_at.
	SetStatus(ctx context.Context, id, status string) error

	// Detach clears a subject's session (back to NULL) and zeroes claudePID,
	// freeing both the (session, scope) slot and the window binding so a fresh
	// subject can take them.
	Detach(ctx context.Context, id string) error

	// Get returns the subject by id, or an error if no such subject exists.
	Get(ctx context.Context, id string) (Subject, error)
}
