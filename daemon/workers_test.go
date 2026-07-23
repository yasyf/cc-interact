package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerGroupRejectsStartsAfterCloseAndBoundsWait(t *testing.T) {
	workers := newWorkerGroup()
	release := make(chan struct{})
	if !workers.Start(func() { <-release }) {
		t.Fatal("initial worker was rejected")
	}
	workers.Close()
	if workers.Start(func() {}) {
		t.Fatal("worker started after close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := workers.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait err = %v, want deadline", err)
	}
	close(release)
	settleCtx, settleCancel := context.WithTimeout(context.Background(), time.Second)
	defer settleCancel()
	if err := workers.Wait(settleCtx); err != nil {
		t.Fatalf("settled Wait: %v", err)
	}
}

func TestRuntimeActivationHonorsCancellationBeforePublishingWorkers(t *testing.T) {
	s := newTestServer(t, Config{})
	workers := &serverWorkers{owner: s}
	workers.Close()
	workers.Cancel()
	called := false
	s.bootReconcile = func(context.Context, *Server) error {
		called = true
		return nil
	}
	err := s.activateServing(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve err = %v, want context cancellation", err)
	}
	if called {
		t.Fatal("boot reconciliation ran after worker cancellation")
	}
}
