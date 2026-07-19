package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yasyf/cc-interact/subject"
)

// gateConfig blocks edits while a subject's status is "open" and records every
// observed verdict.
func gateConfig(observed *[]bool) Config {
	return Config{
		GateErrorReason: "fail-closed: status unreadable",
		Gate: func(_ context.Context, s subject.Subject, _ ToolCall) (bool, string) {
			if s.Status == "open" {
				return false, "an open review blocks edits"
			}
			return true, ""
		},
		GateObserve: func(_ context.Context, _ subject.Subject, _ ToolCall, allow bool, _ string) {
			*observed = append(*observed, allow)
		},
	}
}

func TestGuardEditBlocksOpenSubject(t *testing.T) {
	var observed []bool
	s := newTestServer(t, gateConfig(&observed))
	seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 4242, "open")

	body, _ := json.Marshal(guardEditBody{ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"x.go"}`)})
	r := s.dispatch(context.Background(), Envelope{
		Op: OpGuardEdit, Scope: "scopeA", Session: "sess1", ClaudePID: 4242, Body: body,
	})
	if !r.OK || r.Allow || r.Reason == "" {
		t.Fatalf("guard-edit on open subject = %+v, want block with reason", r)
	}
	if len(observed) != 1 || observed[0] != false {
		t.Fatalf("GateObserve = %v, want one block", observed)
	}
}

func TestGuardEditAllowsClosedSubject(t *testing.T) {
	var observed []bool
	s := newTestServer(t, gateConfig(&observed))
	seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 4242, "closed")

	r := s.dispatch(context.Background(), Envelope{
		Op: OpGuardEdit, Scope: "scopeA", Session: "sess1", ClaudePID: 4242,
	})
	if !r.OK || !r.Allow || r.Reason != "" {
		t.Fatalf("guard-edit on closed subject = %+v, want allow", r)
	}
	if len(observed) != 1 || observed[0] != true {
		t.Fatalf("GateObserve = %v, want one allow", observed)
	}
}

func TestGuardEditNoSubjectAllows(t *testing.T) {
	var observed []bool
	s := newTestServer(t, gateConfig(&observed))
	r := s.dispatch(context.Background(), Envelope{
		Op: OpGuardEdit, Scope: "scopeA", Session: "sess1", ClaudePID: 4242,
	})
	if !r.OK || !r.Allow {
		t.Fatalf("guard-edit with no subject = %+v, want allow (nothing to guard)", r)
	}
	if len(observed) != 0 {
		t.Fatalf("GateObserve fired %v times with no subject, want 0", observed)
	}
}

func TestGuardEditFailsClosedOnResolveError(t *testing.T) {
	var observed []bool
	s := newTestServer(t, gateConfig(&observed))
	seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 4242, "open")
	s.store.Close() // force the resolver's query to fail

	r := s.dispatch(context.Background(), Envelope{
		Op: OpGuardEdit, Scope: "scopeA", Session: "sess1", ClaudePID: 4242,
	})
	if !r.OK || r.Allow || r.Reason != "fail-closed: status unreadable" {
		t.Fatalf("guard-edit on resolve error = %+v, want fail-closed block", r)
	}
}
