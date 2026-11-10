package daemon

import (
	"maps"
	"testing"
	"time"
)

func TestActivityAttachDetachCounts(t *testing.T) {
	a := NewActivity()
	_, d1 := a.Attach("s1", "channel", 100, 0)
	_, d2 := a.Attach("s1", "channel", 100, 0)

	if !a.Attached("s1", "channel", 100) {
		t.Fatal("attached after two attaches")
	}
	d1()
	if !a.Attached("s1", "channel", 100) {
		t.Fatal("one open connection must still count as attached")
	}
	d2()
	d2() // double detach must not underflow
	if a.Attached("s1", "channel", 100) {
		t.Fatal("detached after both connections closed")
	}
	if a.Attached("s1", "watch", 100) || a.Attached("s2", "channel", 100) || a.Attached("s1", "channel", 200) {
		t.Fatal("attachment leaked across consumer, subject, or window")
	}
}

func TestActivityCounts(t *testing.T) {
	a := NewActivity()
	_, d1 := a.Attach("s1", "watch", 100, 0)
	_, d2 := a.Attach("s1", "watch", 200, 0)
	_, overlap := a.Attach("s1", "watch", 100, 0)
	_, dChannel := a.Attach("s1", "channel", 100, 0)
	_, dOther := a.Attach("s2", "watch", 100, 0)
	defer dOther()

	for _, tc := range []struct {
		name string
		drop func()
		want map[string]int
	}{
		{"connections sum across pids and reconnect overlap", func() {}, map[string]int{"watch": 3, "channel": 1}},
		{"overlap detach decrements", overlap, map[string]int{"watch": 2, "channel": 1}},
		{"first pid detach decrements", d1, map[string]int{"watch": 1, "channel": 1}},
		{"duplicate detach does not underflow", d1, map[string]int{"watch": 1, "channel": 1}},
		{"second pid detach removes consumer", d2, map[string]int{"channel": 1}},
		{"last detach empties counts", dChannel, map[string]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.drop()
			if got := a.Counts("s1"); !maps.Equal(got, tc.want) {
				t.Fatalf("Counts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActivityPolledSinceWindow(t *testing.T) {
	a := NewActivity()
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }

	a.NotePoll("/scope", "channel", 100)
	if !a.PolledSince("/scope", "channel", 100, 3*time.Second) {
		t.Fatal("fresh poll must count")
	}
	now = now.Add(2 * time.Second)
	if !a.PolledSince("/scope", "channel", 100, 3*time.Second) {
		t.Fatal("poll within the window must count")
	}
	now = now.Add(2 * time.Second)
	if a.PolledSince("/scope", "channel", 100, 3*time.Second) {
		t.Fatal("poll outside the window must not count")
	}
	if a.PolledSince("/scope", "watch", 100, time.Hour) || a.PolledSince("/scope", "channel", 200, time.Hour) {
		t.Fatal("poll leaked across consumers or windows")
	}
}

func TestActivityProven(t *testing.T) {
	a := NewActivity()
	if a.Proven(100) {
		t.Fatal("never-acked window must read unproven")
	}
	a.MarkProven(100)
	if !a.Proven(100) {
		t.Fatal("acked window must read proven")
	}
	if a.Proven(200) {
		t.Fatal("proof leaked across windows")
	}
	_, detach := a.Attach("s1", "channel", 100, 0)
	detach()
	if !a.Proven(100) {
		t.Fatal("proof is daemon-lifetime and must survive an SSE detach")
	}
}

func TestActivityAttachedWithinGrace(t *testing.T) {
	a := NewActivity()
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }

	if a.AttachedWithin("s1", 10*time.Second) {
		t.Fatal("never-attached subject must read unattached")
	}
	_, dChannel := a.Attach("s1", "channel", 100, 0)
	_, dWatch := a.Attach("s1", "watch", 200, 0)
	if !a.AttachedWithin("s1", 10*time.Second) {
		t.Fatal("a live attachment of any consumer must count")
	}

	dChannel()
	now = now.Add(time.Hour)
	if !a.AttachedWithin("s1", 10*time.Second) {
		t.Fatal("the surviving attachment must keep the subject occupied")
	}

	dWatch()
	now = now.Add(5 * time.Second)
	if !a.AttachedWithin("s1", 10*time.Second) {
		t.Fatal("a drop within the grace must still read occupied")
	}
	now = now.Add(6 * time.Second)
	if a.AttachedWithin("s1", 10*time.Second) {
		t.Fatal("a drop past the grace must read unattached")
	}
	if a.AttachedWithin("s2", time.Hour) {
		t.Fatal("grace leaked across subjects")
	}
}

func TestPushedSpanBelongsToTheOneOpenStream(t *testing.T) {
	a := NewActivity()
	if _, _, ok := a.Pushed("s1", "channel", 100); ok {
		t.Fatal("a window with no stream reported a pushed span")
	}
	pushedA, detachA := a.Attach("s1", "channel", 100, 100)
	pushedA(140)
	if after, through, ok := a.Pushed("s1", "channel", 100); !ok || after != 100 || through != 140 {
		t.Fatalf("pushed = (%d, %d], %v; want (100, 140], true", after, through, ok)
	}
	_, detachB := a.Attach("s1", "channel", 100, 0)
	if _, _, ok := a.Pushed("s1", "channel", 100); ok {
		t.Fatal("overlapping streams reported one pushed span")
	}
	detachB()
	if after, through, ok := a.Pushed("s1", "channel", 100); !ok || after != 100 || through != 140 {
		t.Fatalf("after the overlap pushed = (%d, %d], %v; want the remaining stream's (100, 140], true", after, through, ok)
	}
	detachA()
	if _, _, ok := a.Pushed("s1", "channel", 100); ok {
		t.Fatal("a detached stream still reported a pushed span")
	}
}
