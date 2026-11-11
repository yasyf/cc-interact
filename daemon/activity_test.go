package daemon

import (
	"maps"
	"testing"
	"time"
)

func TestActivityAttachDetachCounts(t *testing.T) {
	a := NewActivity()
	_, _, d1 := a.Attach("s1", "channel", 100, 0, 0)
	_, _, d2 := a.Attach("s1", "channel", 100, 0, 0)

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
	_, _, d1 := a.Attach("s1", "watch", 100, 0, 0)
	_, _, d2 := a.Attach("s1", "watch", 200, 0, 0)
	_, _, overlap := a.Attach("s1", "watch", 100, 0, 0)
	_, _, dChannel := a.Attach("s1", "channel", 100, 0, 0)
	_, _, dOther := a.Attach("s2", "watch", 100, 0, 0)
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

func TestActivityAttachedWithinGrace(t *testing.T) {
	a := NewActivity()
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }

	if a.AttachedWithin("s1", 10*time.Second) {
		t.Fatal("never-attached subject must read unattached")
	}
	_, _, dChannel := a.Attach("s1", "channel", 100, 0, 0)
	_, _, dWatch := a.Attach("s1", "watch", 200, 0, 0)
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
	pushedA, _, detachA := a.Attach("s1", "channel", 100, 100, 0)
	pushedA(140)
	if after, through, ok := a.Pushed("s1", "channel", 100); !ok || after != 100 || through != 140 {
		t.Fatalf("pushed = (%d, %d], %v; want (100, 140], true", after, through, ok)
	}
	_, _, detachB := a.Attach("s1", "channel", 100, 0, 0)
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

func TestALaterStartedProcessHoldsTheWindowsStream(t *testing.T) {
	closed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	a := NewActivity()
	_, legacy, detachLegacy := a.Attach("s1", "channel", 100, 0, 0)
	_, older, detachOlder := a.Attach("s1", "channel", 100, 0, 1000)
	_, overlap, detachOverlap := a.Attach("s1", "channel", 100, 0, 1000)
	if !closed(legacy) || closed(older) || closed(overlap) {
		t.Fatal("a started process must supersede the window's legacy stream and keep its own reconnect overlap")
	}
	_, otherWindow, detachOtherWindow := a.Attach("s1", "channel", 200, 0, 500)
	_, pidless, detachPidless := a.Attach("s1", "channel", 0, 0, 500)
	_, laterPidless, detachLaterPidless := a.Attach("s1", "channel", 0, 0, 3000)

	pushed, newer, detachNewer := a.Attach("s1", "channel", 100, 40, 2000)
	if !closed(older) || !closed(overlap) || closed(newer) {
		t.Fatal("a later-started process must supersede every earlier stream in its window")
	}
	if closed(otherWindow) || closed(pidless) || closed(laterPidless) {
		t.Fatal("supersede leaked across windows or onto pid-less consumers")
	}
	_, stale, _ := a.Attach("s1", "channel", 100, 0, 1500)
	if !closed(stale) || closed(newer) {
		t.Fatal("an earlier-started process attaching after a later one must be refused")
	}
	if got := a.Counts("s1")["channel"]; got != 7 {
		t.Fatalf("channel count = %d, want 7: superseded streams count until they detach, a refused one never does", got)
	}

	detachOtherWindow()
	detachPidless()
	detachLaterPidless()
	pushed(55)
	if after, through, ok := a.Pushed("s1", "channel", 100); !ok || after != 40 || through != 55 {
		t.Fatalf("pushed = (%d, %d], %v; want the later-started stream's (40, 55], true before the superseded ones detach", after, through, ok)
	}
	detachLegacy()
	detachOlder()
	detachOverlap()
	if got := a.Counts("s1"); !maps.Equal(got, map[string]int{"channel": 1}) {
		t.Fatalf("Counts = %v, want the later-started stream alone", got)
	}
	detachNewer()
	if a.Attached("s1", "channel", 100) {
		t.Fatal("detached after the last stream closed")
	}
}
