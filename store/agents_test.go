package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

func TestListPendingDirectiveAgents(t *testing.T) {
	ctx := context.Background()
	s, subjects := openTestStore(t)
	subjectA := create(t, subjects, "session-a", "/repo/a", 0)
	subjectB := create(t, subjects, "session-b", "/repo/b", 0)

	mk := func(subjectID, agentID string, done bool, pending, delivered int) {
		t.Helper()
		if _, err := s.RegisterAgent(ctx, agentInfo(subjectID, agentID, time.Unix(100, 0))); err != nil {
			t.Fatalf("register %s/%s: %v", subjectID, agentID, err)
		}
		for i := 0; i < delivered; i++ {
			if _, _, err := s.EnqueueDirective(ctx, subjectID, agentID, "human", "delivered", time.Unix(150, 0)); err != nil {
				t.Fatalf("enqueue delivered %s/%s: %v", subjectID, agentID, err)
			}
		}
		if delivered > 0 {
			if _, err := s.DrainDirectives(ctx, subjectID, agentID, time.Unix(160, 0)); err != nil {
				t.Fatalf("drain delivered %s/%s: %v", subjectID, agentID, err)
			}
		}
		for i := 0; i < pending; i++ {
			if _, _, err := s.EnqueueDirective(ctx, subjectID, agentID, "human", "pending", time.Unix(170, 0)); err != nil {
				t.Fatalf("enqueue pending %s/%s: %v", subjectID, agentID, err)
			}
		}
		if done {
			if _, err := s.CloseAgent(ctx, subjectID, agentID, time.Unix(200, 0)); err != nil {
				t.Fatalf("close %s/%s: %v", subjectID, agentID, err)
			}
		}
	}

	// Two done+pending agents in subject A exercise intra-subject ordering.
	mk(subjectA.ID, "a-1-done-pending", true, 1, 0)
	mk(subjectA.ID, "a-0-done-pending", true, 2, 1)
	mk(subjectA.ID, "a-done-delivered", true, 0, 2)   // done, all delivered → excluded
	mk(subjectA.ID, "a-running-pending", false, 1, 0) // running, pending → excluded
	mk(subjectB.ID, "b-done-pending", true, 1, 0)     // second subject → cross-subject ordering

	got, err := s.ListPendingDirectiveAgents(ctx, "")
	if err != nil {
		t.Fatalf("list pending directive agents: %v", err)
	}

	type pair struct{ subjectID, agentID string }
	want := []pair{
		{subjectA.ID, "a-0-done-pending"},
		{subjectA.ID, "a-1-done-pending"},
		{subjectB.ID, "b-done-pending"},
	}
	if subjectB.ID < subjectA.ID {
		want = []pair{
			{subjectB.ID, "b-done-pending"},
			{subjectA.ID, "a-0-done-pending"},
			{subjectA.ID, "a-1-done-pending"},
		}
	}
	if len(got) != len(want) {
		t.Fatalf("pending directive agents = %d, want %d (done+pending only): %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].SubjectID != w.subjectID || got[i].AgentID != w.agentID {
			t.Fatalf("pending agent %d = %s/%s, want %s/%s (ordered by subject then agent)",
				i, got[i].SubjectID, got[i].AgentID, w.subjectID, w.agentID)
		}
	}

	// The peek is non-destructive: the pending rows are still drainable afterward.
	drained, err := s.DrainDirectives(ctx, subjectA.ID, "a-1-done-pending", time.Unix(300, 0))
	if err != nil {
		t.Fatalf("drain after peek: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("drained %d after peek, want 1 (peek must not mark delivered)", len(drained))
	}
}

func agentInfo(subjectID, agentID string, startedAt time.Time) agent.Info {
	return agent.Info{
		SubjectID:      subjectID,
		AgentID:        agentID,
		ParentAgentID:  agent.TopLevel,
		AgentType:      "worker",
		SessionID:      "session-1",
		TranscriptPath: "/tmp/transcript-1.jsonl",
		Status:         agent.StatusRunning,
		StartedAt:      startedAt,
	}
}

func TestAgentStore(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "register refreshes mutable fields without changing start time",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				startedAt := time.Unix(100, 0)
				info := agentInfo(subject.ID, "worker-1", startedAt)
				if _, err := s.RegisterAgent(ctx, info); err != nil {
					t.Fatalf("register agent: %v", err)
				}
				if _, err := s.CloseAgent(ctx, subject.ID, info.AgentID, time.Unix(200, 0)); err != nil {
					t.Fatalf("close agent: %v", err)
				}

				info.ParentAgentID = "new-parent"
				info.AgentType = "researcher"
				info.SessionID = "session-2"
				info.TranscriptPath = "/tmp/transcript-2.jsonl"
				info.StartedAt = time.Unix(300, 0)
				if _, err := s.RegisterAgent(ctx, info); err != nil {
					t.Fatalf("re-register agent: %v", err)
				}

				got, err := s.GetAgent(ctx, subject.ID, info.AgentID)
				if err != nil {
					t.Fatalf("get agent: %v", err)
				}
				if !got.StartedAt.Equal(startedAt) {
					t.Fatalf("started_at = %v, want %v", got.StartedAt, startedAt)
				}
				if got.Status != agent.StatusRunning {
					t.Fatalf("status = %q, want %q", got.Status, agent.StatusRunning)
				}
				if !got.EndedAt.IsZero() {
					t.Fatalf("ended_at = %v, want zero", got.EndedAt)
				}
				if got.ParentAgentID != info.ParentAgentID ||
					got.AgentType != info.AgentType ||
					got.SessionID != info.SessionID ||
					got.TranscriptPath != info.TranscriptPath {
					t.Fatalf("mutable fields = %+v, want %+v", got, info)
				}
			},
		},
		{
			name: "close unknown agent fails",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				_, err := s.CloseAgent(ctx, subject.ID, "missing", time.Unix(100, 0))
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("close unknown agent err = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "get unknown agent returns ErrNotFound",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				if _, err := s.GetAgent(ctx, subject.ID, "missing"); err != ErrNotFound {
					t.Fatalf("get unknown agent err = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "list orders agents by start time",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				for _, info := range []agent.Info{
					agentInfo(subject.ID, "later", time.Unix(200, 0)),
					agentInfo(subject.ID, "earlier", time.Unix(100, 0)),
				} {
					if _, err := s.RegisterAgent(ctx, info); err != nil {
						t.Fatalf("register %s: %v", info.AgentID, err)
					}
				}
				got, err := s.ListAgents(ctx, subject.ID)
				if err != nil {
					t.Fatalf("list agents: %v", err)
				}
				if len(got) != 2 || got[0].AgentID != "earlier" || got[1].AgentID != "later" {
					t.Fatalf("agent order = %+v, want earlier then later", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
