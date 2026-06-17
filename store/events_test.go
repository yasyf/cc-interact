package store

import (
	"context"
	"testing"

	"github.com/yasyf/cc-interact/event"
)

func TestAppendEventSeqAndOriginFilter(t *testing.T) {
	ctx := context.Background()
	s, st := openTestStore(t)
	r := create(t, st, "s", "/repo", 0)

	want := []struct {
		origin string
		seq    int64
	}{{event.OriginHuman, 1}, {event.OriginAgent, 2}, {event.OriginHuman, 3}}
	for _, w := range want {
		e := &event.Event{SubjectID: r.ID, Origin: w.origin, Type: "t", Payload: []byte(`{"x":1}`)}
		seq, err := s.AppendEvent(ctx, e)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if seq != w.seq {
			t.Fatalf("seq = %d, want %d", seq, w.seq)
		}
		if e.Seq != w.seq {
			t.Fatalf("event Seq not stamped: %d, want %d", e.Seq, w.seq)
		}
	}

	all, _ := s.EventsSince(ctx, r.ID, 0, "")
	if len(all) != 3 {
		t.Fatalf("all events = %d, want 3", len(all))
	}
	noAgent, _ := s.EventsSince(ctx, r.ID, 0, event.OriginAgent)
	if len(noAgent) != 2 {
		t.Fatalf("excludeOrigin=agent events = %d, want 2", len(noAgent))
	}
	for _, e := range noAgent {
		if e.Origin == event.OriginAgent {
			t.Fatal("agent event leaked through the filter")
		}
	}
	// A real per-origin filter, not a hardcoded agent filter: excluding human drops
	// only the two human rows and keeps the lone agent row.
	noHuman, _ := s.EventsSince(ctx, r.ID, 0, event.OriginHuman)
	if len(noHuman) != 1 {
		t.Fatalf("excludeOrigin=human events = %d, want 1", len(noHuman))
	}
	if noHuman[0].Origin != event.OriginAgent {
		t.Fatalf("excludeOrigin=human kept origin %q, want agent", noHuman[0].Origin)
	}

	since := all[1].Seq // 2
	tail, _ := s.EventsSince(ctx, r.ID, since, "")
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("events since %d = %+v, want only seq 3", since, tail)
	}
}

func TestAppendEventPerSubjectSeq(t *testing.T) {
	ctx := context.Background()
	s, st := openTestStore(t)
	a := create(t, st, "sa", "/repo/a", 0)
	b := create(t, st, "sb", "/repo/b", 0)

	for _, id := range []string{a.ID, a.ID, b.ID} {
		if _, err := s.AppendEvent(ctx, &event.Event{SubjectID: id, Origin: event.OriginHuman, Type: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	bEvents, _ := s.EventsSince(ctx, b.ID, 0, "")
	if len(bEvents) != 1 || bEvents[0].Seq != 1 {
		t.Fatalf("subject b events = %+v, want a single seq 1 (per-subject seq)", bEvents)
	}
	aEvents, _ := s.EventsSince(ctx, a.ID, 0, "")
	if len(aEvents) != 2 || aEvents[1].Seq != 2 {
		t.Fatalf("subject a events = %+v, want seqs 1,2", aEvents)
	}
}

func TestEventPayloadDefaultsToEmptyObject(t *testing.T) {
	ctx := context.Background()
	s, st := openTestStore(t)
	r := create(t, st, "s", "/repo", 0)

	if _, err := s.AppendEvent(ctx, &event.Event{SubjectID: r.ID, Origin: event.OriginSystem, Type: "t"}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.EventsSince(ctx, r.ID, 0, "")
	if len(all) != 1 || string(all[0].Payload) != "{}" {
		t.Fatalf("payload = %q, want {}", string(all[0].Payload))
	}
}

func TestEventDedupReturnsExistingSeq(t *testing.T) {
	ctx := context.Background()
	s, st := openTestStore(t)
	r := create(t, st, "s", "/repo", 0)

	first, _ := s.AppendEvent(ctx, &event.Event{SubjectID: r.ID, Origin: event.OriginHuman, Type: "t", DedupKey: "dk"})
	second, _ := s.AppendEvent(ctx, &event.Event{SubjectID: r.ID, Origin: event.OriginHuman, Type: "t", DedupKey: "dk"})
	if first != second {
		t.Fatalf("dedup seq mismatch: %d vs %d", first, second)
	}
	all, _ := s.EventsSince(ctx, r.ID, 0, "")
	if len(all) != 1 {
		t.Fatalf("got %d events, want 1 (deduped)", len(all))
	}
}

func TestEventDedupScopedBySubject(t *testing.T) {
	ctx := context.Background()
	s, st := openTestStore(t)
	a := create(t, st, "sa", "/repo/a", 0)
	b := create(t, st, "sb", "/repo/b", 0)

	// A dedup key reused across subjects must not collide: each subject keeps its
	// own seq space and its own dedup namespace.
	if _, err := s.AppendEvent(ctx, &event.Event{SubjectID: a.ID, Origin: event.OriginHuman, Type: "t", DedupKey: "k"}); err != nil {
		t.Fatal(err)
	}
	seqB, err := s.AppendEvent(ctx, &event.Event{SubjectID: b.ID, Origin: event.OriginHuman, Type: "t", DedupKey: "k"})
	if err != nil {
		t.Fatalf("same dedup key on a different subject must insert: %v", err)
	}
	if seqB != 1 {
		t.Fatalf("subject b seq = %d, want 1", seqB)
	}
}
