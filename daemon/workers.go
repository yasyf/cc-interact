package daemon

import (
	"context"
	"sync"
)

type workerGroup struct {
	mu      sync.Mutex
	active  int
	closed  bool
	drained chan struct{}
}

func newWorkerGroup() *workerGroup {
	return &workerGroup{drained: make(chan struct{})}
}

func (g *workerGroup) Start(fn func()) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.active++
	g.mu.Unlock()
	go func() {
		defer g.done()
		fn()
	}()
	return true
}

func (g *workerGroup) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	if g.active == 0 {
		close(g.drained)
	}
}

func (g *workerGroup) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.drained:
		return nil
	}
}

func (g *workerGroup) done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active < 0 {
		panic("daemon: negative worker count")
	}
	if g.closed && g.active == 0 {
		close(g.drained)
	}
}
