package sse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testIntake struct {
	mu      sync.Mutex
	active  int
	closed  bool
	drained chan struct{}
}

func newTestIntake() *testIntake { return &testIntake{drained: make(chan struct{})} }

func (i *testIntake) Admit() (func(), error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil, context.Canceled
	}
	i.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			i.mu.Lock()
			defer i.mu.Unlock()
			i.active--
			if i.closed && i.active == 0 {
				close(i.drained)
			}
		})
	}, nil
}

func (i *testIntake) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	if i.active == 0 {
		close(i.drained)
	}
}

func (i *testIntake) Wait(ctx context.Context) error {
	select {
	case <-i.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestDrainWaitsForInflightStream proves an in-flight SSE stream holds the drain
// open: the drain's settle blocks until the stream ends, and a new stream is
// refused while draining.
func TestDrainWaitsForInflightStream(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1")
	d := newTestIntake()
	var storeClosed atomic.Bool
	released := make(chan bool, 1)
	srv := startServer(t, b, Config{Admit: func() (func(), error) {
		release, err := d.Admit()
		if err != nil {
			return nil, err
		}
		return func() {
			released <- !storeClosed.Load()
			release()
		}, nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := connectUntilLive(t, ctx, srv.URL+"/events?session=s1")
	defer func() { _ = resp.Body.Close() }()

	drained := make(chan error, 1)
	var teardown sync.WaitGroup
	teardown.Add(1)
	go func() {
		defer teardown.Done()
		d.Close()
		drained <- d.Wait(context.Background())
	}()
	closed := make(chan struct{})
	go func() {
		teardown.Wait()
		storeClosed.Store(true)
		close(closed)
	}()

	select {
	case <-drained:
		t.Fatal("Drain completed while an SSE stream was in-flight")
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := d.Admit(); err == nil {
		t.Fatal("Admit succeeded after Drain began; want a draining refusal")
	}

	cancel() // end the stream: the handler returns and releases its admission
	select {
	case open := <-released:
		if !open {
			t.Fatal("SSE stream release observed a closed store")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream did not release its admission")
	}

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not complete after the SSE stream ended")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("store close did not follow the drain barrier")
	}
}

func TestClosePresenceHoldsTheStoreForAnAttachThatOutlivesDrain(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1id")
	d := newTestIntake()
	var storeClosed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	sawOpen := make(chan bool, 1)
	s := NewServer(b, Config{
		PresenceDebounce: time.Hour,
		Admit:            d.Admit,
		OnPresenceChange: func(_ context.Context, _ string, connected bool) {
			if !connected {
				return
			}
			close(entered)
			<-release
			sawOpen <- !storeClosed.Load()
		},
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?session=s1id&consumer=channel&claude_pid=4242", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the attach never reached OnPresenceChange")
	}

	d.Close()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer drainCancel()
	if err := d.Wait(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain wait = %v, want the attach to outlive its budget", err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer closeCancel()
	if err := s.ClosePresence(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ClosePresence() = %v while the attach still emits, want %v", err, context.DeadlineExceeded)
	}

	closed := make(chan error, 1)
	go func() {
		err := s.ClosePresence(context.Background())
		storeClosed.Store(true)
		closed <- err
	}()
	select {
	case err := <-closed:
		t.Fatalf("ClosePresence() = %v before the attach returned", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case open := <-sawOpen:
		if !open {
			t.Fatal("the attach's OnPresenceChange ran after the store closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the attach never finished")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("ClosePresence() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ClosePresence never returned after the attach finished")
	}
}
