package daemon

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestChannelState(t *testing.T) {
	s := newTestServer(t, Config{})
	pid := os.Getpid()
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", pid, "open")
	hc := HandlerCtx{Env: Envelope{ClaudePID: pid}, Scope: "scopeA"}
	if got := s.ChannelState(hc, sub.ID, "channel"); got != ChannelInactive {
		t.Fatalf("no consumer = %q, want inactive", got)
	}
	s.activity.NotePoll("scopeA", "channel", pid)
	if got := s.ChannelState(hc, sub.ID, "channel"); got != ChannelPending {
		t.Fatalf("polling consumer = %q, want pending", got)
	}
	_, detach := s.activity.Attach(sub.ID, "channel", pid, 0)
	defer detach()
	if got := s.ChannelState(hc, sub.ID, "channel"); got != ChannelPending {
		t.Fatalf("attached unproven consumer = %q, want pending", got)
	}
	if err := s.recordProof(os.Getppid()); err != nil {
		t.Fatal(err)
	}
	if got := s.ChannelState(hc, sub.ID, "channel"); got != ChannelPending {
		t.Fatalf("another window's proof = %q, want pending", got)
	}
	prove(t, s, sub)
	if got := s.ChannelState(hc, sub.ID, "channel"); got != ChannelActive {
		t.Fatalf("attached proven consumer = %q, want active", got)
	}
	if got := s.ChannelState(hc, sub.ID, "watch"); got != ChannelInactive {
		t.Fatalf("proof without the consumer attached = %q, want inactive", got)
	}
}

func TestDispatchStatusReportsProof(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id3", "slug3", "sess3", "scopeC", os.Getpid(), "open")
	status := func() StatusBody {
		r := s.dispatch(context.Background(), Envelope{Op: OpStatus, Scope: "scopeC", Session: "sess3", ClaudePID: os.Getpid()})
		var body StatusBody
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("unmarshal status body: %v", err)
		}
		return body
	}
	if status().Proven {
		t.Fatal("status before channel-ack reports proven")
	}
	prove(t, s, sub)
	if !status().Proven {
		t.Fatal("status after channel-ack must report proven")
	}
}
