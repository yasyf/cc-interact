package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

func TestEnqueueDirective(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "unknown explicit agent fails",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				_, _, err := s.EnqueueDirective(ctx, subject.ID, "missing", "human", "work", time.Unix(100, 0))
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("enqueue unknown agent err = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "implicit top-level agent succeeds",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				now := time.Unix(100, 0)
				directive, status, err := s.EnqueueDirective(
					ctx, subject.ID, agent.TopLevel, "human", "work", now)
				if err != nil {
					t.Fatalf("enqueue top-level directive: %v", err)
				}
				if status != agent.StatusRunning {
					t.Fatalf("top-level status = %q, want %q", status, agent.StatusRunning)
				}
				if directive.ID == 0 || directive.SubjectID != subject.ID ||
					directive.AgentID != agent.TopLevel || directive.Origin != "human" ||
					directive.Text != "work" || !directive.CreatedAt.Equal(now) ||
					!directive.DeliveredAt.IsZero() {
					t.Fatalf("directive = %+v, want inserted pending directive", directive)
				}
			},
		},
		{
			name: "returns running and done status from enqueue transaction",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				info := agentInfo(subject.ID, "worker-1", time.Unix(100, 0))
				if _, err := s.RegisterAgent(ctx, info); err != nil {
					t.Fatalf("register agent: %v", err)
				}

				cases := []struct {
					name       string
					wantStatus string
					before     func(t *testing.T)
				}{
					{name: "running", wantStatus: agent.StatusRunning, before: func(t *testing.T) {}},
					{
						name:       "done",
						wantStatus: agent.StatusDone,
						before: func(t *testing.T) {
							if _, err := s.CloseAgent(ctx, subject.ID, info.AgentID, time.Unix(200, 0)); err != nil {
								t.Fatalf("close agent: %v", err)
							}
						},
					},
				}
				for i, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						tc.before(t)
						_, status, err := s.EnqueueDirective(
							ctx, subject.ID, info.AgentID, "human", tc.name, time.Unix(int64(300+i), 0))
						if err != nil {
							t.Fatalf("enqueue directive: %v", err)
						}
						if status != tc.wantStatus {
							t.Fatalf("status = %q, want %q", status, tc.wantStatus)
						}
					})
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestDrainDirectives(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "drains FIFO and marks delivered",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subject := create(t, subjects, "session-1", "/repo", 0)
				info := agentInfo(subject.ID, "worker-1", time.Unix(50, 0))
				if _, err := s.RegisterAgent(ctx, info); err != nil {
					t.Fatalf("register agent: %v", err)
				}
				for i, directive := range []struct {
					text string
					now  time.Time
				}{
					{text: "first", now: time.Unix(100, 0)},
					{text: "second", now: time.Unix(100, 0)},
					{text: "third", now: time.Unix(101, 0)},
				} {
					if _, _, err := s.EnqueueDirective(
						ctx, subject.ID, info.AgentID, "human", directive.text, directive.now); err != nil {
						t.Fatalf("enqueue directive %d: %v", i, err)
					}
				}

				deliveredAt := time.Unix(200, 0)
				got, err := s.DrainDirectives(ctx, subject.ID, info.AgentID, deliveredAt)
				if err != nil {
					t.Fatalf("drain directives: %v", err)
				}
				if len(got) != 3 {
					t.Fatalf("drained directives = %d, want 3", len(got))
				}
				for i, want := range []string{"first", "second", "third"} {
					if got[i].Text != want {
						t.Fatalf("directive %d text = %q, want %q", i, got[i].Text, want)
					}
					if !got[i].DeliveredAt.Equal(deliveredAt) {
						t.Fatalf("directive %d delivered_at = %v, want %v", i, got[i].DeliveredAt, deliveredAt)
					}
				}

				second, err := s.DrainDirectives(ctx, subject.ID, info.AgentID, time.Unix(300, 0))
				if err != nil {
					t.Fatalf("second drain: %v", err)
				}
				if second == nil || len(second) != 0 {
					t.Fatalf("second drain = %+v, want non-nil empty slice", second)
				}
			},
		},
		{
			name: "only drains addressed subject and agent",
			run: func(t *testing.T) {
				ctx := context.Background()
				s, subjects := openTestStore(t)
				subjectA := create(t, subjects, "session-a", "/repo/a", 0)
				subjectB := create(t, subjects, "session-b", "/repo/b", 0)
				for _, info := range []agent.Info{
					agentInfo(subjectA.ID, "worker-1", time.Unix(50, 0)),
					agentInfo(subjectA.ID, "worker-2", time.Unix(50, 0)),
					agentInfo(subjectB.ID, "worker-1", time.Unix(50, 0)),
				} {
					if _, err := s.RegisterAgent(ctx, info); err != nil {
						t.Fatalf("register %s/%s: %v", info.SubjectID, info.AgentID, err)
					}
					if _, _, err := s.EnqueueDirective(
						ctx, info.SubjectID, info.AgentID, "human", info.SubjectID+"/"+info.AgentID, time.Unix(100, 0)); err != nil {
						t.Fatalf("enqueue %s/%s: %v", info.SubjectID, info.AgentID, err)
					}
				}

				got, err := s.DrainDirectives(ctx, subjectA.ID, "worker-1", time.Unix(200, 0))
				if err != nil {
					t.Fatalf("drain addressed agent: %v", err)
				}
				if len(got) != 1 || got[0].Text != subjectA.ID+"/worker-1" {
					t.Fatalf("addressed drain = %+v, want subject A worker 1", got)
				}

				for _, address := range []struct {
					subjectID string
					agentID   string
				}{
					{subjectID: subjectA.ID, agentID: "worker-2"},
					{subjectID: subjectB.ID, agentID: "worker-1"},
				} {
					pending, err := s.DrainDirectives(
						ctx, address.subjectID, address.agentID, time.Unix(201, 0))
					if err != nil {
						t.Fatalf("drain %s/%s: %v", address.subjectID, address.agentID, err)
					}
					if len(pending) != 1 {
						t.Fatalf("pending %s/%s = %+v, want one", address.subjectID, address.agentID, pending)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestConcurrentEnqueueAndClose(t *testing.T) {
	tests := []struct {
		name     string
		enqueues int
	}{
		{name: "every running observation remains drainable", enqueues: 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, subjects := openTestStore(t)
			subject := create(t, subjects, "session-1", "/repo", 0)
			info := agentInfo(subject.ID, "worker-1", time.Unix(50, 0))
			if _, err := s.RegisterAgent(ctx, info); err != nil {
				t.Fatalf("register agent: %v", err)
			}

			type result struct {
				directive agent.Directive
				status    string
				err       error
			}
			start := make(chan struct{})
			results := make(chan result, tt.enqueues)
			closeResult := make(chan error, 1)
			for i := range tt.enqueues {
				go func(i int) {
					<-start
					directive, status, err := s.EnqueueDirective(
						ctx,
						subject.ID,
						info.AgentID,
						"human",
						fmt.Sprintf("directive-%d", i),
						time.Unix(int64(100+i), 0),
					)
					results <- result{directive: directive, status: status, err: err}
				}(i)
			}
			go func() {
				<-start
				_, err := s.CloseAgent(ctx, subject.ID, info.AgentID, time.Unix(1000, 0))
				closeResult <- err
			}()
			close(start)

			if err := <-closeResult; err != nil {
				t.Fatalf("close agent: %v", err)
			}
			enqueued := make([]result, 0, tt.enqueues)
			for range tt.enqueues {
				got := <-results
				if got.err != nil {
					t.Fatalf("enqueue directive: %v", got.err)
				}
				if got.status != agent.StatusRunning && got.status != agent.StatusDone {
					t.Fatalf("enqueue status = %q, want running or done", got.status)
				}
				enqueued = append(enqueued, got)
			}

			drained, err := s.DrainDirectives(ctx, subject.ID, info.AgentID, time.Unix(2000, 0))
			if err != nil {
				t.Fatalf("drain directives: %v", err)
			}
			if len(drained) != tt.enqueues {
				t.Fatalf("drained %d directives, want %d", len(drained), tt.enqueues)
			}
			drainedIDs := make(map[int64]bool, len(drained))
			for _, directive := range drained {
				drainedIDs[directive.ID] = true
			}
			for _, got := range enqueued {
				if !drainedIDs[got.directive.ID] {
					t.Fatalf("directive %d observed %q but was not drainable", got.directive.ID, got.status)
				}
			}
		})
	}
}
