package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-interact/subject"
)

func openTestStore(t *testing.T) (*Store, subject.Store) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, NewSubjectStore(s.DB())
}

func newID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("read random id: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// create inserts an open subject with a fresh id (id doubles as its slug) and
// returns it; the subject store never mints ids, so the test supplies them.
func create(t *testing.T, st subject.Store, session, scope string, pid int) subject.Subject {
	t.Helper()
	id := newID(t)
	s, err := st.Create(context.Background(), id, id, session, scope, pid, "open")
	if err != nil {
		t.Fatalf("create subject: %v", err)
	}
	return s
}

func TestOpenRunsMigrate(t *testing.T) {
	migrate := func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS widgets (id TEXT PRIMARY KEY)`)
		return err
	}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), migrate)
	if err != nil {
		t.Fatalf("open with migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.DB().Exec(`INSERT INTO widgets(id) VALUES('w1')`); err != nil {
		t.Fatalf("migrate did not create widgets table: %v", err)
	}
}

func TestSubjectResolution(t *testing.T) {
	ctx := context.Background()
	_, st := openTestStore(t)

	r := create(t, st, "sess-1", "/repo/a", 1234)
	if r.Status != "open" {
		t.Fatalf("status = %q, want open", r.Status)
	}
	if r.ClaudePID != 1234 {
		t.Fatalf("claude_pid = %d, want 1234", r.ClaudePID)
	}

	got, ok, err := st.FindBySessionScope(ctx, "sess-1", "/repo/a")
	if err != nil || !ok {
		t.Fatalf("find by session/scope: ok=%v err=%v", ok, err)
	}
	if got.ID != r.ID {
		t.Fatalf("found id %q, want %q", got.ID, r.ID)
	}
	if got.ClaudePID != 1234 {
		t.Fatalf("persisted claude_pid = %d, want 1234", got.ClaudePID)
	}

	if _, ok, _ := st.FindBySessionScope(ctx, "sess-2", "/repo/a"); ok {
		t.Fatal("different session should not match")
	}
	if _, ok, _ := st.FindBySessionScope(ctx, "", "/repo/a"); ok {
		t.Fatal("blank session should never match")
	}

	// Session-less subjects must not collide on the partial unique index.
	if _, err := st.Create(ctx, newID(t), "", "", "/repo/b", 0, "open"); err != nil {
		t.Fatalf("session-less subject 1: %v", err)
	}
	if _, err := st.Create(ctx, newID(t), "", "", "/repo/c", 0, "open"); err != nil {
		t.Fatalf("session-less subject 2: %v", err)
	}
}

func TestFindLatestByWindowScope(t *testing.T) {
	ctx := context.Background()
	_, st := openTestStore(t)

	older := create(t, st, "s1", "/repo", 1234)
	newer := create(t, st, "s2", "/repo", 1234)

	got, ok, err := st.FindLatestByWindowScope(ctx, 1234, "/repo")
	if err != nil || !ok {
		t.Fatalf("find by window/scope: ok=%v err=%v", ok, err)
	}
	if got.ID != newer.ID {
		t.Fatalf("found id %q, want newest %q (older was %q)", got.ID, newer.ID, older.ID)
	}

	if _, ok, _ := st.FindLatestByWindowScope(ctx, 1234, "/other"); ok {
		t.Fatal("different scope should not match")
	}
	if _, ok, _ := st.FindLatestByWindowScope(ctx, 9999, "/repo"); ok {
		t.Fatal("different pid should not match")
	}
	if _, ok, err := st.FindLatestByWindowScope(ctx, 0, "/repo"); ok || err != nil {
		t.Fatalf("pid 0 must never match: ok=%v err=%v", ok, err)
	}
}

func TestRebind(t *testing.T) {
	ctx := context.Background()
	s, st := openTestStore(t)

	t.Run("success rebinds session and pid and bumps updated_at", func(t *testing.T) {
		r := create(t, st, "s1", "/repo/a", 1234)
		if _, err := s.DB().ExecContext(ctx, `UPDATE subjects SET updated_at=1 WHERE id=?`, r.ID); err != nil {
			t.Fatal(err)
		}
		ok, err := st.Rebind(ctx, r.ID, 1234, "s2", 5678)
		if err != nil || !ok {
			t.Fatalf("rebind: ok=%v err=%v", ok, err)
		}
		got, _ := st.Get(ctx, r.ID)
		if got.SessionID != "s2" || got.ClaudePID != 5678 {
			t.Fatalf("got session=%q pid=%d, want s2/5678", got.SessionID, got.ClaudePID)
		}
		if got.UpdatedAt.Unix() <= 1 {
			t.Fatalf("updated_at not bumped: %v", got.UpdatedAt)
		}
		if _, ok, _ := st.FindLatestByWindowScope(ctx, 5678, "/repo/a"); !ok {
			t.Fatal("new pid should now find the subject")
		}
	})

	t.Run("wrong fromPID is a clean CAS miss", func(t *testing.T) {
		r := create(t, st, "s3", "/repo/b", 1234)
		ok, err := st.Rebind(ctx, r.ID, 9999, "s4", 5678)
		if err != nil {
			t.Fatalf("cas miss must not error: %v", err)
		}
		if ok {
			t.Fatal("cas with stale fromPID should not land")
		}
		got, _ := st.Get(ctx, r.ID)
		if got.SessionID != "s3" || got.ClaudePID != 1234 {
			t.Fatalf("binding changed on a missed cas: session=%q pid=%d", got.SessionID, got.ClaudePID)
		}
	})

	t.Run("session occupying the scope slot propagates the unique violation", func(t *testing.T) {
		if _, err := st.Create(ctx, newID(t), newID(t), "s5", "/repo/c", 100, "open"); err != nil {
			t.Fatal(err)
		}
		b := create(t, st, "s6", "/repo/c", 200)
		if _, err := st.Rebind(ctx, b.ID, 200, "s5", 300); err == nil {
			t.Fatal("rebind onto an occupied (session, scope) slot should fail")
		}
		got, _ := st.Get(ctx, b.ID)
		if got.SessionID != "s6" || got.ClaudePID != 200 {
			t.Fatalf("binding changed despite failed rebind: session=%q pid=%d", got.SessionID, got.ClaudePID)
		}
	})

	t.Run("no status gate: non-active subjects rebind", func(t *testing.T) {
		r := create(t, st, "s7", "/repo/d", 1234)
		if err := st.SetStatus(ctx, r.ID, "submitted"); err != nil {
			t.Fatal(err)
		}
		ok, err := st.Rebind(ctx, r.ID, 1234, "s8", 5678)
		if err != nil || !ok {
			t.Fatalf("rebind submitted subject: ok=%v err=%v", ok, err)
		}
		got, _ := st.Get(ctx, r.ID)
		if got.SessionID != "s8" || got.ClaudePID != 5678 || got.Status != "submitted" {
			t.Fatalf("got session=%q pid=%d status=%q, want s8/5678/submitted", got.SessionID, got.ClaudePID, got.Status)
		}
	})
}

func TestDetachFreesSessionAndWindow(t *testing.T) {
	ctx := context.Background()
	_, st := openTestStore(t)
	r := create(t, st, "s1", "/repo", 1234)

	if err := st.Detach(ctx, r.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got, _ := st.Get(ctx, r.ID)
	if got.SessionID != "" || got.ClaudePID != 0 {
		t.Fatalf("got session=%q pid=%d, want detached", got.SessionID, got.ClaudePID)
	}
	if _, ok, _ := st.FindLatestByWindowScope(ctx, 1234, "/repo"); ok {
		t.Fatal("detached subject must not be pid-findable")
	}

	// Both slots are free: the same session and window can own a fresh subject.
	fresh := create(t, st, "s1", "/repo", 1234)
	found, ok, _ := st.FindLatestByWindowScope(ctx, 1234, "/repo")
	if !ok || found.ID != fresh.ID {
		t.Fatalf("window should find fresh subject %q, got %q (ok=%v)", fresh.ID, found.ID, ok)
	}
}

func TestGetMissing(t *testing.T) {
	ctx := context.Background()
	_, st := openTestStore(t)
	if _, err := st.Get(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
}
