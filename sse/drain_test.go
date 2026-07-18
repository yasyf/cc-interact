package sse

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/drain"
)

// TestDrainWaitsForInflightStream proves an in-flight SSE stream holds the drain
// open: the drain's settle blocks until the stream ends, and a new stream is
// refused while draining.
func TestDrainWaitsForInflightStream(t *testing.T) {
	b := newFakeBackend()
	b.addSubject("s1")
	d := &drain.Simple{}
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
		drained <- d.Drain(context.Background(), drain.SimpleConfig{
			Deactivate:      func(context.Context) error { return nil },
			MarkClosing:     func() {},
			CancelExecutors: func() {},
		})
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
