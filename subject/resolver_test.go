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
// session, pid 0 never matches by window, "latest" is the most recently inserted
// matching row, and Rebind is a compare-and-swap on claude_pid.
type fakeStore struct {
	rows []*Subject
	ord  map[string]int64
	next int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{ord: map[string]int64{}}
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

func newResolver() (Resolver, *fakeStore) {
	f := newFakeStore()
	rs := Resolver{
		Store:  f,
		Policy: Policy{Active: func(s Subject) bool { return s.Status == "open" }},
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
			name: "another window's open review is never adopted; a second window creates its own",
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "open")
			},
			w: Window{Session: "sB", ClaudePID: 200},
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, got Subject) {
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sA" || pid != 100 {
					t.Fatalf("foreign review disturbed: %s/%d, want sA/100", sess, pid)
				}
				if got.SessionID != "sB" || got.ClaudePID != 200 {
					t.Fatalf("created %s/%d, want sB/200", got.SessionID, got.ClaudePID)
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
			name: "fresh with another window's open review still creates its own",
			seed: func(t *testing.T, ctx context.Context, _ Resolver, f *fakeStore) Subject {
				return seedSubject(t, ctx, f, "sA", 100, "open")
			},
			w:     Window{Session: "sB", ClaudePID: 200},
			fresh: true,
			after: func(t *testing.T, ctx context.Context, f *fakeStore, seeded, _ Subject) {
				if sess, pid := bindingOf(t, ctx, f, seeded.ID); sess != "sA" || pid != 100 {
					t.Fatalf("foreign review disturbed: %s/%d", sess, pid)
				}
				if s, _ := f.Get(ctx, seeded.ID); s.Status != "open" {
					t.Fatalf("foreign status = %q, want open", s.Status)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs, f := newResolver()
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
			rs, f := newResolver()
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

func TestRebind(t *testing.T) {
	cases := []struct {
		name       string
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
			name:     "another window's review is never rebound",
			seedSess: "sA", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "sB", ClaudePID: 200},
			wantSess: "sA", wantPID: 100,
		},
		{
			name:     "empty session id is a no-op",
			seedSess: "sA", seedPID: 100, seedStatus: "open",
			w:        Window{Session: "", ClaudePID: 200},
			wantSess: "sA", wantPID: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs, f := newResolver()
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
