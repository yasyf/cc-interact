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

	interrupted, err := s.CloseOpenTurnsForWindow(ctx, "/repo", 100)
	if err != nil {
		t.Fatalf("close open turns: %v", err)
	}
	if interrupted != 1 {
		t.Fatalf("interrupted = %d, want 1", interrupted)
	}
	if again, err := s.CloseOpenTurnsForWindow(ctx, "/repo", 100); err != nil || again != 0 {
		t.Fatalf("second close = %d (err %v), want 0", again, err)
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

func setStartedAt(t *testing.T, s *TurnStore, id, ms int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `UPDATE turns SET started_at=? WHERE id=?`, ms, id); err != nil {
		t.Fatalf("restamp turn %d: %v", id, err)
	}
}

func closeTurn(t *testing.T, s *TurnStore, id int64, treeEnd string) {
	t.Helper()
	if err := s.CloseTurn(context.Background(), id, treeEnd, "closed"); err != nil {
		t.Fatalf("close turn %d: %v", id, err)
	}
}

func TestLatestClosedTurn(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, s *TurnStore) int64
		since int64
	}{
		{
			name:  "an empty ledger chains nothing",
			setup: func(t *testing.T, s *TurnStore) int64 { return 0 },
		},
		{
			name: "an open turn has no closing tree",
			setup: func(t *testing.T, s *TurnStore) int64 {
				createTurn(t, s, "/repo", 100, "tree-a")
				return 0
			},
		},
		{
			name: "an interrupted turn has no closing tree",
			setup: func(t *testing.T, s *TurnStore) int64 {
				createTurn(t, s, "/repo", 100, "tree-a")
				if _, err := s.CloseOpenTurnsForWindow(context.Background(), "/repo", 100); err != nil {
					t.Fatalf("interrupt: %v", err)
				}
				return 0
			},
		},
		{
			name: "the newest closed turn is the tip",
			setup: func(t *testing.T, s *TurnStore) int64 {
				first := createTurn(t, s, "/repo", 100, "tree-a")
				second := createTurn(t, s, "/repo", 200, "tree-b")
				closeTurn(t, s, first.ID, "end-a")
				closeTurn(t, s, second.ID, "end-b")
				return second.ID
			},
		},
		{
			name: "a turn older than the window is out of reach",
			setup: func(t *testing.T, s *TurnStore) int64 {
				turn := createTurn(t, s, "/repo", 100, "tree-a")
				closeTurn(t, s, turn.ID, "end-a")
				setStartedAt(t, s, turn.ID, 100)
				return 0
			},
			since: 200,
		},
		{
			name: "another repository's turn never chains",
			setup: func(t *testing.T, s *TurnStore) int64 {
				turn := createTurn(t, s, "/other", 100, "tree-a")
				closeTurn(t, s, turn.ID, "end-a")
				return 0
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestTurnStore(t)
			wantID := tt.setup(t, s)

			got, ok, err := s.LatestClosedTurn(context.Background(), "/repo", tt.since)
			if err != nil {
				t.Fatalf("latest closed: %v", err)
			}
			if ok != (wantID != 0) {
				t.Fatalf("ok = %v (turn %d), want %v", ok, got.ID, wantID != 0)
			}
			if !ok {
				return
			}
			if got.ID != wantID {
				t.Fatalf("turn = %d, want %d", got.ID, wantID)
			}
			if got.TreeEnd != "end-b" {
				t.Fatalf("tree_end = %q, want %q", got.TreeEnd, "end-b")
			}
		})
	}
}

func TestOpenTurnCount(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, s *TurnStore)
		want  int
	}{
		{
			name:  "an empty ledger counts none",
			setup: func(t *testing.T, s *TurnStore) {},
		},
		{
			name: "open turns of two windows both count",
			setup: func(t *testing.T, s *TurnStore) {
				createTurn(t, s, "/repo", 100, "tree-a")
				createTurn(t, s, "/repo", 200, "tree-b")
			},
			want: 2,
		},
		{
			name: "a closed turn stops counting",
			setup: func(t *testing.T, s *TurnStore) {
				closeTurn(t, s, createTurn(t, s, "/repo", 100, "tree-a").ID, "end-a")
				createTurn(t, s, "/repo", 100, "tree-b")
			},
			want: 1,
		},
		{
			name: "an interrupted turn stops counting",
			setup: func(t *testing.T, s *TurnStore) {
				createTurn(t, s, "/repo", 100, "tree-a")
				if _, err := s.CloseOpenTurnsForWindow(context.Background(), "/repo", 100); err != nil {
					t.Fatalf("interrupt: %v", err)
				}
			},
		},
		{
			name: "another repository's open turn is out of scope",
			setup: func(t *testing.T, s *TurnStore) {
				createTurn(t, s, "/other", 100, "tree-a")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestTurnStore(t)
			tt.setup(t, s)

			got, err := s.OpenTurnCount(context.Background(), "/repo")
			if err != nil {
				t.Fatalf("open turn count: %v", err)
			}
			if got != tt.want {
				t.Fatalf("open turns = %d, want %d", got, tt.want)
			}
		})
	}
}
