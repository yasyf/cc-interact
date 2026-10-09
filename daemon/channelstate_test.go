package daemon

import (
	"context"
	"encoding/json"
	"testing"
)

func TestChannelState(t *testing.T) {
	s := newTestServer(t, Config{})
	hc := HandlerCtx{Env: Envelope{ClaudePID: 42}, Scope: "scopeA"}
	if got := s.ChannelState(hc, "id1", "channel"); got != ChannelInactive {
		t.Fatalf("no consumer = %q, want inactive", got)
	}
	s.activity.NotePoll("scopeA", "channel", 42)
	if got := s.ChannelState(hc, "id1", "channel"); got != ChannelPending {
		t.Fatalf("polling consumer = %q, want pending", got)
	}
	detach := s.activity.Attach("id1", "channel", 42)
	defer detach()
	if got := s.ChannelState(hc, "id1", "channel"); got != ChannelPending {
		t.Fatalf("attached unproven consumer = %q, want pending", got)
	}
	s.activity.MarkProven(43)
	if got := s.ChannelState(hc, "id1", "channel"); got != ChannelPending {
		t.Fatalf("another window's proof = %q, want pending", got)
	}
	s.activity.MarkProven(42)
	if got := s.ChannelState(hc, "id1", "channel"); got != ChannelActive {
		t.Fatalf("attached proven consumer = %q, want active", got)
	}
	if got := s.ChannelState(hc, "id1", "watch"); got != ChannelInactive {
		t.Fatalf("proof without the consumer attached = %q, want inactive", got)
	}
}

func TestDispatchStatusReportsProof(t *testing.T) {
	s := newTestServer(t, Config{})
	seedSubject(t, s, "id3", "slug3", "sess3", "scopeC", 11, "open")
	status := func() StatusBody {
		r := s.dispatch(context.Background(), Envelope{Op: OpStatus, Scope: "scopeC", Session: "sess3", ClaudePID: 11})
		var body StatusBody
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("unmarshal status body: %v", err)
		}
		return body
	}
	if status().Proven {
		t.Fatal("status before channel-ack reports proven")
	}
	if r := s.dispatch(context.Background(), Envelope{Op: OpChannelAck, ClaudePID: 11}); !r.OK {
		t.Fatalf("channel-ack = %+v", r)
	}
	if !status().Proven {
		t.Fatal("status after channel-ack must report proven")
	}
}
