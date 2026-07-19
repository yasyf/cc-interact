package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

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
				if err := s.RegisterAgent(ctx, info); err != nil {
					t.Fatalf("register agent: %v", err)
				}
				if err := s.CloseAgent(ctx, subject.ID, info.AgentID, time.Unix(200, 0)); err != nil {
					t.Fatalf("close agent: %v", err)
				}

				info.ParentAgentID = "new-parent"
				info.AgentType = "researcher"
				info.SessionID = "session-2"
				info.TranscriptPath = "/tmp/transcript-2.jsonl"
				info.StartedAt = time.Unix(300, 0)
				if err := s.RegisterAgent(ctx, info); err != nil {
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
				err := s.CloseAgent(ctx, subject.ID, "missing", time.Unix(100, 0))
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
					if err := s.RegisterAgent(ctx, info); err != nil {
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
