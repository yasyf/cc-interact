package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/subject"
)

// TestHeld pins the adoption veto: a subject is Held while its window is alive or
// its channel dropped within heldGrace, and becomes adoptable only once both are
// gone — the guard that stops a concurrent session from stealing a review during
// a --resume gap, when the old window's pid is already dead but its channel only
// just disconnected.
func TestHeld(t *testing.T) {
	const pid = 100
	sub := subject.Subject{ID: "R", ClaudePID: pid}
	base := time.Unix(1_000, 0)

	t.Run("live window holds outright", func(t *testing.T) {
		s := &Server{windowAlive: func(int) bool { return true }, activity: NewActivity()}
		if !s.held(context.Background(), sub) {
			t.Fatal("a live window must hold its subject")
		}
	})

	t.Run("dead window still held while the channel drop is within grace", func(t *testing.T) {
		act := NewActivity()
		act.now = func() time.Time { return base }
		act.Attach(sub.ID, "channel", pid)() // attach then drop stamps lastDrop at base
		act.now = func() time.Time { return base.Add(heldGrace - time.Second) }

		s := &Server{windowAlive: func(int) bool { return false }, activity: act}
		if !s.held(context.Background(), sub) {
			t.Fatal("a channel that dropped within heldGrace must still hold the subject")
		}
	})

	t.Run("dead window becomes adoptable once the grace lapses", func(t *testing.T) {
		act := NewActivity()
		act.now = func() time.Time { return base }
		act.Attach(sub.ID, "channel", pid)()
		act.now = func() time.Time { return base.Add(heldGrace + time.Second) }

		s := &Server{windowAlive: func(int) bool { return false }, activity: act}
		if s.held(context.Background(), sub) {
			t.Fatal("once the window is dead and the grace has lapsed the subject must be adoptable")
		}
	})
}
