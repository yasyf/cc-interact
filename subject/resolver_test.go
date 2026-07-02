package subject

import (
	"context"
	"fmt"
	"testing"
)

const repo = "/repo"

var lc = Lifecycle{Initial: "open", Closed: "closed"}

// fakeStore is the in-memory double for the resolver's Store boundary. It mirrors
// the SQL semantics the resolver depends on: NULL session never matches by
// session, pid 0 never matches by window, "latest" is the most recently
// inserted matching row, adoptable rows are those whose status is in active, and
// Rebind is a compare-and-swap on claude_pid.
type fakeStore struct {
	rows   []*Subject
	ord    map[string]int64
	next   int64
	active map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{ord: map[string]int64{}, active: map[string]bool{"open": true}}
}

func (f *fakeStore) row(id string) *Subject {
	for _, r := range f.rows {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f *fakeStore) latest(pred func(*Subject) bool) (Subject, bool, error) {
	var best *Subject
	var bestOrd int64
	for _, r := range f.rows {
		if pred(r) && (best == nil || f.ord[r.ID] > bestOrd) {
			best, bestOrd = r, f.ord[r.ID]
		}
	}
	if best == nil {
		return Subject{}, false, nil
	}
	return *best, true, nil
}

func (f *fakeStore) FindBySessionScope(_ context.Context, session, scope string) (Subject, bool, error) {
	if session == "" {
		return Subject{}, false, nil
	}
	for _, r := range f.rows {
		if r.SessionID == session && r.Scope == scope {
			return *r, true, nil
		}
	}
	return Subject{}, false, nil
}

func (f *fakeStore) FindLatestByWindowScope(_ context.Context, claudePID int, scope string) (Subject, bool, error) {
	if claudePID == 0 {
		return Subject{}, false, nil
	}
	return f.latest(func(r *Subject) bool { return r.ClaudePID == claudePID && r.Scope == scope })
}

func (f *fakeStore) FindAdoptableByScope(_ context.Context, scope string) (Subject, bool, error) {
	return f.latest(func(r *Subject) bool { return r.Scope == scope && f.active[r.Status] })
}

func (f *fakeStore) Create(_ context.Context, id, slug, session, scope string, claudePID int, status string) (Subject, error) {
	s := Subject{ID: id, Slug: slug, SessionID: session, Scope: scope, ClaudePID: claudePID, Status: status}
	f.next++
	f.ord[id] = f.next
	f.rows = append(f.rows, &s)
	return s, nil
}

func (f *fakeStore) Rebind(_ context.Context, id string, fromPID int, session string, claudePID int) (bool, error) {
	r := f.row(id)
	if r == nil || r.ClaudePID != fromPID {
		return false, nil
	}
	r.SessionID = session
	r.ClaudePID = claudePID
	return true, nil
}

func (f *fakeStore) SetStatus(_ context.Context, id, status string) error {
	r := f.row(id)
	if r == nil {
		return fmt.Errorf("set status: subject %s not found", id)
	}
	r.Status = status
	return nil
}

func (f *fakeStore) Detach(_ context.Context, id string) error {
	r := f.row(id)
	if r == nil {
		return fmt.Errorf("detach: subject %s not found", id)
	}
	r.SessionID = ""
	r.ClaudePID = 0
	return nil
}

func (f *fakeStore) Get(_ context.Context, id string) (Subject, error) {
	r := f.row(id)
	if r == nil {
		return Subject{}, fmt.Errorf("get: subject %s not found", id)
	}
	return *r, nil
}

func newResolver(alive map[int]bool) (Resolver, *fakeStore) {
	f := newFakeStore()
	rs := Resolver{
		Store: f,
		Policy: Policy{
			Held:   func(_ context.Context, s Subject) bool { return alive[s.ClaudePID] },
			Active: func(s Subject) bool { return s.Status == "open" },
		},
	}
	return rs, f
}

func seedSubject(t *testing.T, ctx context.Context, f *fakeStore, session string, pid int, status string) Subject {
	t.Helper()
	s, err := f.Create(ctx, newID(), "slug", session, repo, pid, "open")
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if status != "open" {
		if err := f.SetStatus(ctx, s.ID, status); err != nil {
			t.Fatalf("seed status: %v", err)
		}
		s.Status = status
	}
	return s
}

func bindingOf(t *testing.T, ctx context.Context, f *fakeStore, id string) (string, int) {
	t.Helper()
	s, err := f.Get(ctx, id)
	if err != nil {
		t.Fatalf("get subject: %v", err)
	}
	return s.SessionID, s.ClaudePID
}

func TestStart(t *testing.T) {
	cases := []struct {
		name        string
		alive       map[int]bool
		seed        func(t *testing.T, ctx context.Context, rs Resolver, f *fakeStore) Subject
		w           Window
		fresh       bool
		wantResumed bool
		wantSeeded  bool
		after       func(t *testing.T, ctx context.Context, f *fakeStore, seeded, got Subject)
	}{
		{
			name: "no subject creates one bound to the window",
			w:    Window{Session: "s1", ClaudePID: 100},
			after: func(t *testing.T, ctx context.Context, f *fakeStore, _, got Subject) {
				if got.SessionID != "s1" || got.ClaudePID != 100 || got.Status != "open" {
					t.Fatalf("created session=%q pid=%d status=%q, want s1/100/open", got.SessionID, got.ClaudePID, got.Status)
				}
				if s, err := f.Get(ctx, got.ID); err != nil || s.Slug != got.Slug {
					t.Fatalf("persisted slug = %q (err %v), want %q", s.Slug, err, got.Slug)
				}
			},
		},
		{
			name: "same window resumes its subject",
			seed: func(t *testing.T, ctx context.Context, rs Resolver, _ *fakeStore) Subject {
				s, resumed, err := rs.Start(ctx, Window{Session: "s1", ClaudePID: 100}, repo, "main", lc, false)
				if err != nil || resumed {
					t.Fatalf("seed start: resumed=%v err=%v", resumed, err)
				}
				return s
			},
			w:           Window{Session: "s1", ClaudePID: 100},
			wantResumed: true,
			wantSeeded:  true,
		},
		{
			name:  "second live window creates its own, first binding untouched",
			alive: map[int]bool{100: true},
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "open")
			},
			w: Window{Session: "sB", ClaudePID: 200},
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, _ Subject) {
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sA" || pid != 100 {
					t.Fatalf("first binding disturbed: %s/%d", sess, pid)
				}
			},
		},
		{
			name: "rotation: new session id same pid rebinds and resumes",
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "open")
			},
			w:           Window{Session: "sB", ClaudePID: 100},
			wantResumed: true,
			wantSeeded:  true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, got Subject) {
				if got.SessionID != "sB" {
					t.Fatalf("returned session = %q, want sB", got.SessionID)
				}
				if _, ok, _ := f.FindBySessionScope(ctx, "sA", repo); ok {
					t.Fatal("old session id still bound")
				}
				if s, ok, _ := f.FindBySessionScope(ctx, "sB", repo); !ok || s.ID != seeded.ID {
					t.Fatal("new session id not bound to the subject")
				}
			},
		},
		{
			name: "exact session match with stale pid is refreshed",
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "s1", 0, "open")
			},
			w:           Window{Session: "s1", ClaudePID: 999},
			wantResumed: true,
			wantSeeded:  true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, got Subject) {
				if got.ClaudePID != 999 {
					t.Fatalf("returned pid = %d, want 999", got.ClaudePID)
				}
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "s1" || pid != 999 {
					t.Fatalf("binding = %s/%d, want s1/999", sess, pid)
				}
			},
		},
		{
			name: "submitted window subject resumes after rotation",
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "submitted")
			},
			w:           Window{Session: "sB", ClaudePID: 100},
			wantResumed: true,
			wantSeeded:  true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, _ Subject) {
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sB" || pid != 100 {
					t.Fatalf("binding = %s/%d, want sB/100", sess, pid)
				}
				if s, _ := f.Get(ctx, seeded.ID); s.Status != "submitted" {
					t.Fatalf("status = %q, want submitted", s.Status)
				}
			},
		},
		{
			name:  "orphaned active subject adopted when its window is dead",
			alive: map[int]bool{100: false},
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "open")
			},
			w:           Window{Session: "sB", ClaudePID: 200},
			wantResumed: true,
			wantSeeded:  true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, got Subject) {
				if got.SessionID != "sB" || got.ClaudePID != 200 {
					t.Fatalf("returned %s/%d, want sB/200", got.SessionID, got.ClaudePID)
				}
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sB" || pid != 200 {
					t.Fatalf("binding = %s/%d, want sB/200", sess, pid)
				}
			},
		},
		{
			name: "dead window's submitted subject is not adopted",
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "submitted")
			},
			w: Window{Session: "sB", ClaudePID: 200},
		},
		{
			name:  "blank-pid subject never cross-adopted by another blank-pid window",
			alive: map[int]bool{0: true},
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 0, "open")
			},
			w: Window{Session: "sB", ClaudePID: 0},
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, _ Subject) {
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sA" || pid != 0 {
					t.Fatalf("binding = %s/%d, want sA/0", sess, pid)
				}
			},
		},
		{
			name: "blank session id still creates",
			w:    Window{},
			after: func(t *testing.T, ctx context.Context, f *fakeStore, _, got Subject) {
				if got.SessionID != "" || got.ClaudePID != 0 {
					t.Fatalf("created %s/%d, want blank/0", got.SessionID, got.ClaudePID)
				}
			},
		},
		{
			name: "fresh closes and detaches own subject then creates",
			seed: func(t *testing.T, ctx context.Context, rs Resolver, _ *fakeStore) Subject {
				s, _, err := rs.Start(ctx, Window{Session: "s1", ClaudePID: 100}, repo, "main", lc, false)
				if err != nil {
					t.Fatalf("seed start: %v", err)
				}
				return s
			},
			w:     Window{Session: "s1", ClaudePID: 100},
			fresh: true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, got Subject) {
				if s, _ := f.Get(ctx, seeded.ID); s.Status != "closed" {
					t.Fatalf("old status = %q, want closed", s.Status)
				}
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "" || pid != 0 {
					t.Fatalf("old binding = %s/%d, want detached", sess, pid)
				}
				if s, ok, _ := f.FindBySessionScope(ctx, "s1", repo); !ok || s.ID != got.ID {
					t.Fatal("fresh subject does not own the session slot")
				}
			},
		},
		{
			name:  "fresh never adopts an orphan",
			alive: map[int]bool{100: false},
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "open")
			},
			w:     Window{Session: "sB", ClaudePID: 200},
			fresh: true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, _ Subject) {
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sA" || pid != 100 {
					t.Fatalf("orphan binding disturbed: %s/%d", sess, pid)
				}
				if s, _ := f.Get(ctx, seeded.ID); s.Status != "open" {
					t.Fatalf("orphan status = %q, want open", s.Status)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs, f := newResolver(tc.alive)
			var seeded Subject
			if tc.seed != nil {
				seeded = tc.seed(t, ctx, rs, f)
			}

			got, resumed, err := rs.Start(ctx, tc.w, repo, "main", lc, tc.fresh)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if resumed != tc.wantResumed {
				t.Fatalf("resumed = %v, want %v", resumed, tc.wantResumed)
			}
			if same := got.ID == seeded.ID; same != tc.wantSeeded {
				t.Fatalf("got id %q (seeded %q), want seeded=%v", got.ID, seeded.ID, tc.wantSeeded)
			}
			if tc.after != nil {
				tc.after(t, ctx, f, seeded, got)
			}
		})
	}
}

func TestFind(t *testing.T) {
	cases := []struct {
		name       string
		seedSess   string
		seedPID    int
		seedStatus string
		w          Window
		wantOK     bool
	}{
		{"exact session binding", "s1", 100, "open", Window{Session: "s1", ClaudePID: 100}, true},
		{"rotated session id falls through to pid", "sA", 100, "open", Window{Session: "sB", ClaudePID: 100}, true},
		{"submitted subject found by pid after rotation", "sA", 100, "submitted", Window{Session: "sB", ClaudePID: 100}, true},
		{"no binding for a different window", "sA", 100, "open", Window{Session: "sB", ClaudePID: 200}, false},
		{"pid 0 never matches by window", "sA", 0, "open", Window{Session: "sB", ClaudePID: 0}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs, f := newResolver(nil)
			seeded := seedSubject(t, ctx, f, tc.seedSess, tc.seedPID, tc.seedStatus)

			got, ok, err := rs.Find(ctx, tc.w, repo)
			if err != nil {
				t.Fatalf("find: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.ID != seeded.ID {
				t.Fatalf("found id %q, want %q", got.ID, seeded.ID)
			}
			if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != tc.seedSess || pid != tc.seedPID {
				t.Fatalf("find wrote: binding now %s/%d", sess, pid)
			}
		})
	}
}

func TestPeek(t *testing.T) {
	cases := []struct {
		name       string
		alive      map[int]bool
		seedSess   string
		seedPID    int
		seedStatus string
		w          Window
		wantOK     bool
	}{
		{"exact session binding", nil, "s1", 100, "open", Window{Session: "s1", ClaudePID: 100}, true},
		{"rotated session id falls through to pid", nil, "sA", 100, "open", Window{Session: "sB", ClaudePID: 100}, true},
		{"dead window's active subject is the adoption candidate", map[int]bool{100: false}, "sA", 100, "open", Window{Session: "sB", ClaudePID: 200}, true},
		{"live foreign window's subject is not peeked", map[int]bool{100: true}, "sA", 100, "open", Window{Session: "sB", ClaudePID: 200}, false},
		{"dead window's submitted subject is not adopted", nil, "sA", 100, "submitted", Window{Session: "sB", ClaudePID: 200}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs, f := newResolver(tc.alive)
			seeded := seedSubject(t, ctx, f, tc.seedSess, tc.seedPID, tc.seedStatus)

			got, ok, err := rs.Peek(ctx, tc.w, repo)
			if err != nil {
				t.Fatalf("peek: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.ID != seeded.ID {
				t.Fatalf("peeked id %q, want %q", got.ID, seeded.ID)
			}
			if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != tc.seedSess || pid != tc.seedPID {
				t.Fatalf("peek wrote: binding now %s/%d", sess, pid)
			}
		})
	}
}

func TestRebind(t *testing.T) {
	cases := []struct {
		name       string
		alive      map[int]bool
		seedSess   string
		seedPID    int
		seedStatus string
		w          Window
		wantSess   string
		wantPID    int
	}{
		{
			name:     "already bound is a no-op",
			seedSess: "s1", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "s1", ClaudePID: 100},
			wantSess: "s1", wantPID: 100,
		},
		{
			name:     "rotation moves binding to the new session id",
			seedSess: "sA", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "sB", ClaudePID: 100},
			wantSess: "sB", wantPID: 100,
		},
		{
			name:     "pid-latest subject not active is skipped",
			seedSess: "sA", seedPID: 100, seedStatus: "submitted",
			w:        Window{Session: "sB", ClaudePID: 100},
			wantSess: "sA", wantPID: 100,
		},
		{
			name:     "dead window's active subject is not adopted by rebind",
			alive:    map[int]bool{100: false},
			seedSess: "sA", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "sB", ClaudePID: 200},
			wantSess: "sA", wantPID: 100,
		},
		{
			name:     "live foreign window never stolen",
			alive:    map[int]bool{100: true},
			seedSess: "sA", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "sB", ClaudePID: 200},
			wantSess: "sA", wantPID: 100,
		},
		{
			name:     "empty session id is a no-op",
			alive:    map[int]bool{100: false},
			seedSess: "sA", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "", ClaudePID: 200},
			wantSess: "sA", wantPID: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs, f := newResolver(tc.alive)
			seeded := seedSubject(t, ctx, f, tc.seedSess, tc.seedPID, tc.seedStatus)

			if err := rs.Rebind(ctx, tc.w, repo); err != nil {
				t.Fatalf("rebind: %v", err)
			}
			if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != tc.wantSess || pid != tc.wantPID {
				t.Fatalf("binding = %s/%d, want %s/%d", sess, pid, tc.wantSess, tc.wantPID)
			}
		})
	}
}

func TestAdoptRace(t *testing.T) {
	steal := func(t *testing.T, f *fakeStore) func(ctx context.Context, s Subject) bool {
		return func(ctx context.Context, s Subject) bool {
			if ok, err := f.Rebind(ctx, s.ID, s.ClaudePID, "winner", 200); err != nil || !ok {
				t.Fatalf("competing rebind: ok=%v err=%v", ok, err)
			}
			return false
		}
	}

	t.Run("start loser falls through and creates its own", func(t *testing.T) {
		ctx := context.Background()
		rs, f := newResolver(nil)
		orphan := seedSubject(t, ctx, f, "sA", 100, "open")
		rs.Policy.Held = steal(t, f)

		got, resumed, err := rs.Start(ctx, Window{Session: "loser", ClaudePID: 300}, repo, "main", lc, false)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if resumed || got.ID == orphan.ID {
			t.Fatalf("loser must create its own: resumed=%v id=%q orphan=%q", resumed, got.ID, orphan.ID)
		}
		if got.SessionID != "loser" || got.ClaudePID != 300 {
			t.Fatalf("created %s/%d, want loser/300", got.SessionID, got.ClaudePID)
		}
		if sess, pid := bindingOf(t, ctx, f, orphan.ID); sess != "winner" || pid != 200 {
			t.Fatalf("orphan binding = %s/%d, want winner/200", sess, pid)
		}
	})

	t.Run("rebind never adopts a foreign orphan", func(t *testing.T) {
		ctx := context.Background()
		rs, f := newResolver(map[int]bool{100: false})
		orphan := seedSubject(t, ctx, f, "sA", 100, "open")

		if err := rs.Rebind(ctx, Window{Session: "sB", ClaudePID: 300}, repo); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		if sess, pid := bindingOf(t, ctx, f, orphan.ID); sess != "sA" || pid != 100 {
			t.Fatalf("orphan binding = %s/%d, want sA/100 (session-record must not adopt)", sess, pid)
		}
		if _, ok, _ := f.FindBySessionScope(ctx, "sB", repo); ok {
			t.Fatal("a rotated/new session must not cross-bind to another window's review")
		}
	})
}
