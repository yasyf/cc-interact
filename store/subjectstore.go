package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yasyf/cc-interact/subject"
)

// ErrNotFound is returned when a lookup by id finds no row.
var ErrNotFound = errors.New("not found")

const subjectCols = `id, slug, session_id, scope, claude_pid, status, created_at, updated_at`

type subjectStore struct {
	db             *sql.DB
	activeStatuses []string
}

// NewSubjectStore returns a subject.Store backed by db's subjects table.
// activeStatuses is the adoptable set FindAdoptableByScope filters on
// (cc-review: {"open"}).
func NewSubjectStore(db *sql.DB, activeStatuses []string) subject.Store {
	return &subjectStore{db: db, activeStatuses: activeStatuses}
}

func scanSubject(row interface{ Scan(...any) error }) (subject.Subject, error) {
	var (
		s       subject.Subject
		session sql.NullString
		created int64
		updated int64
	)
	if err := row.Scan(&s.ID, &s.Slug, &session, &s.Scope, &s.ClaudePID, &s.Status, &created, &updated); err != nil {
		return subject.Subject{}, err
	}
	s.SessionID = session.String
	s.CreatedAt = fromUnix(created)
	s.UpdatedAt = fromUnix(updated)
	return s, nil
}

func (st *subjectStore) FindBySessionScope(ctx context.Context, session, scope string) (subject.Subject, bool, error) {
	if session == "" {
		return subject.Subject{}, false, nil
	}
	row := st.db.QueryRowContext(ctx,
		`SELECT `+subjectCols+` FROM subjects WHERE session_id=? AND scope=?`, session, scope)
	s, err := scanSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return subject.Subject{}, false, nil
	}
	return s, err == nil, err
}

func (st *subjectStore) FindLatestByWindowScope(ctx context.Context, claudePID int, scope string) (subject.Subject, bool, error) {
	if claudePID == 0 {
		return subject.Subject{}, false, nil
	}
	row := st.db.QueryRowContext(ctx,
		`SELECT `+subjectCols+` FROM subjects WHERE claude_pid=? AND scope=? ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		claudePID, scope)
	s, err := scanSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return subject.Subject{}, false, nil
	}
	return s, err == nil, err
}

func (st *subjectStore) FindAdoptableByScope(ctx context.Context, scope string) (subject.Subject, bool, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(st.activeStatuses)), ",")
	args := make([]any, 0, len(st.activeStatuses)+1)
	args = append(args, scope)
	for _, s := range st.activeStatuses {
		args = append(args, s)
	}
	row := st.db.QueryRowContext(ctx,
		`SELECT `+subjectCols+` FROM subjects WHERE scope=? AND status IN (`+placeholders+`) ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		args...)
	s, err := scanSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return subject.Subject{}, false, nil
	}
	return s, err == nil, err
}

func (st *subjectStore) Create(ctx context.Context, id, slug, session, scope string, claudePID int, status string) (subject.Subject, error) {
	now := time.Now()
	s := subject.Subject{ID: id, Slug: slug, SessionID: session, Scope: scope, ClaudePID: claudePID, Status: status, CreatedAt: now, UpdatedAt: now}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO subjects(id, slug, session_id, scope, claude_pid, status, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, slug, nullString(session), scope, claudePID, status, unix(now), unix(now)); err != nil {
		return subject.Subject{}, fmt.Errorf("create subject: %w", err)
	}
	return s, nil
}

func (st *subjectStore) Rebind(ctx context.Context, id string, fromPID int, session string, claudePID int) (bool, error) {
	res, err := st.db.ExecContext(ctx,
		`UPDATE subjects SET session_id=?, claude_pid=?, updated_at=? WHERE id=? AND claude_pid=?`,
		nullString(session), claudePID, unix(time.Now()), id, fromPID)
	if err != nil {
		return false, fmt.Errorf("rebind subject: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rebind subject: %w", err)
	}
	return n == 1, nil
}

func (st *subjectStore) SetStatus(ctx context.Context, id, status string) error {
	_, err := st.db.ExecContext(ctx,
		`UPDATE subjects SET status=?, updated_at=? WHERE id=?`, status, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set subject status: %w", err)
	}
	return nil
}

func (st *subjectStore) Detach(ctx context.Context, id string) error {
	_, err := st.db.ExecContext(ctx,
		`UPDATE subjects SET session_id=NULL, claude_pid=0, updated_at=? WHERE id=?`, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("detach subject: %w", err)
	}
	return nil
}

func (st *subjectStore) Get(ctx context.Context, id string) (subject.Subject, error) {
	row := st.db.QueryRowContext(ctx, `SELECT `+subjectCols+` FROM subjects WHERE id=?`, id)
	s, err := scanSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return subject.Subject{}, ErrNotFound
	}
	return s, err
}
