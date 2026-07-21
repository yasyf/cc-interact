package vcs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-interact/store"
)

func openTestTurnStore(t *testing.T) *TurnStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(t.Context(), dbPath, TurnsSchema())
	if err != nil {
		t.Fatalf("open exact turn store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewTurnStore(st.DB())
}

func createTurn(t *testing.T, s *TurnStore, repoRoot string, claudePID int, treeStart string) Turn {
	t.Helper()
	turn, err := s.CreateTurn(context.Background(), Turn{
		RepoRoot: repoRoot, Backend: "git", SessionID: "sess", ClaudePID: claudePID,
		PromptExcerpt: "fix the bug", TreeStart: treeStart,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return turn
}

func TestTurnLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	turn := createTurn(t, s, "/repo", 100, "tree-start")
	if turn.ID == 0 || turn.Status != "open" || turn.StartedAt == 0 {
		t.Fatalf("created turn = %+v, want id, status=open, started_at stamped", turn)
	}
	if turn.EndedAt != 0 || turn.TreeEnd != "" {
		t.Fatalf("created turn = %+v, want no end state", turn)
	}

	got, ok, err := s.LatestOpenTurn(ctx, "/repo", 100)
	if err != nil || !ok {
		t.Fatalf("latest open: ok=%v err=%v", ok, err)
	}
	if got != turn {
		t.Fatalf("latest open = %+v, want %+v", got, turn)
	}

	if err := s.CloseTurn(ctx, turn.ID, "tree-end", "closed"); err != nil {
		t.Fatalf("close turn: %v", err)
	}
	if _, ok, err := s.LatestOpenTurn(ctx, "/repo", 100); err != nil || ok {
		t.Fatalf("after close: ok=%v err=%v, want absent", ok, err)
	}
	closed, err := s.ListTurnsByIDs(ctx, []int64{turn.ID})
	if err != nil || len(closed) != 1 {
		t.Fatalf("list closed: %v (%d turns)", err, len(closed))
	}
	if closed[0].TreeEnd != "tree-end" || closed[0].Status != "closed" || closed[0].EndedAt == 0 {
		t.Fatalf("closed turn = %+v, want tree_end, status=closed, ended_at stamped", closed[0])
	}
}

func TestLatestOpenTurnPicksNewest(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	first := createTurn(t, s, "/repo", 100, "t1")
	second := createTurn(t, s, "/repo", 100, "t2")
	if second.ID <= first.ID {
		t.Fatalf("ids not increasing: %d then %d", first.ID, second.ID)
	}

	got, ok, err := s.LatestOpenTurn(ctx, "/repo", 100)
	if err != nil || !ok {
		t.Fatalf("latest open: ok=%v err=%v", ok, err)
	}
	if got.ID != second.ID {
		t.Fatalf("latest open id = %d, want %d", got.ID, second.ID)
	}
}

func TestCloseOpenTurnsForWindowScopedToPID(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	mine := createTurn(t, s, "/repo", 100, "t1")
	other := createTurn(t, s, "/repo", 200, "t2")
	elsewhere := createTurn(t, s, "/other", 100, "t3")

	if err := s.CloseOpenTurnsForWindow(ctx, "/repo", 100); err != nil {
		t.Fatalf("close open turns: %v", err)
	}

	turns, err := s.ListTurnsByIDs(ctx, []int64{mine.ID, other.ID, elsewhere.ID})
	if err != nil || len(turns) != 3 {
		t.Fatalf("list: %v (%d turns)", err, len(turns))
	}
	if turns[0].Status != "interrupted" || turns[0].TreeEnd != "" {
		t.Fatalf("mine = %+v, want interrupted with empty tree_end", turns[0])
	}
	if turns[1].Status != "open" {
		t.Fatalf("other pid's turn = %+v, want still open", turns[1])
	}
	if turns[2].Status != "open" {
		t.Fatalf("other repo's turn = %+v, want still open", turns[2])
	}
}

func TestListAttributableTurns(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	before := createTurn(t, s, "/repo", 100, "t1")
	createTurn(t, s, "/elsewhere", 100, "t2")
	inWindow1 := createTurn(t, s, "/repo", 100, "t3")
	inWindow2 := createTurn(t, s, "/repo", 200, "t4")
	for id, startedAt := range map[int64]int64{before.ID: 1000, inWindow1.ID: 2000, inWindow2.ID: 3000} {
		if _, err := s.db.ExecContext(ctx, `UPDATE turns SET started_at=? WHERE id=?`, startedAt, id); err != nil {
			t.Fatalf("pin started_at: %v", err)
		}
	}

	turns, err := s.ListAttributableTurns(ctx, "/repo", 1500)
	if err != nil {
		t.Fatalf("list since 1500: %v", err)
	}
	if len(turns) != 2 || turns[0].ID != inWindow1.ID || turns[1].ID != inWindow2.ID {
		t.Fatalf("windowed turns = %+v, want [%d %d]", turns, inWindow1.ID, inWindow2.ID)
	}

	turns, err = s.ListAttributableTurns(ctx, "/repo", 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(turns) != 3 || turns[0].ID != before.ID || turns[1].ID != inWindow1.ID || turns[2].ID != inWindow2.ID {
		t.Fatalf("all turns = %+v, want repo turns ordered by id", turns)
	}
}

func TestGetTurn(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	turn := createTurn(t, s, "/repo", 100, "t1")
	got, err := s.GetTurn(ctx, turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if got != turn {
		t.Fatalf("turn = %+v, want %+v", got, turn)
	}

	if _, err := s.GetTurn(ctx, turn.ID+999); !errors.Is(err, ErrTurnNotFound) {
		t.Fatalf("missing turn err = %v, want ErrTurnNotFound", err)
	}
}

func TestListTurnsBySession(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	mk := func(session, repo string) Turn {
		t.Helper()
		turn, err := s.CreateTurn(ctx, Turn{
			RepoRoot: repo, Backend: "git", SessionID: session, ClaudePID: 100, TreeStart: "t",
		})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		return turn
	}
	first := mk("sess-a", "/repo")
	mk("sess-b", "/repo")
	second := mk("sess-a", "/other")

	turns, err := s.ListTurnsBySession(ctx, "sess-a")
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(turns) != 2 || turns[0].ID != first.ID || turns[1].ID != second.ID {
		t.Fatalf("turns = %+v, want [%d %d] across repos in ledger order", turns, first.ID, second.ID)
	}

	if turns, err := s.ListTurnsBySession(ctx, "sess-none"); err != nil || len(turns) != 0 {
		t.Fatalf("unknown session: turns=%+v err=%v, want none", turns, err)
	}
}

func TestListTurnsByIDs(t *testing.T) {
	ctx := context.Background()
	s := openTestTurnStore(t)

	t1 := createTurn(t, s, "/repo", 100, "t1")
	createTurn(t, s, "/repo", 100, "t2")
	t3 := createTurn(t, s, "/repo", 100, "t3")

	turns, err := s.ListTurnsByIDs(ctx, []int64{t3.ID, t1.ID})
	if err != nil {
		t.Fatalf("list by ids: %v", err)
	}
	if len(turns) != 2 || turns[0].ID != t1.ID || turns[1].ID != t3.ID {
		t.Fatalf("turns = %+v, want [%d %d] ordered by id", turns, t1.ID, t3.ID)
	}

	turns, err = s.ListTurnsByIDs(ctx, nil)
	if err != nil || turns != nil {
		t.Fatalf("empty ids: turns=%+v err=%v, want none", turns, err)
	}
}
