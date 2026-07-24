package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/trust"
	"github.com/yasyf/daemonkit/wire"
	"github.com/yasyf/daemonkit/worker"
)

type clientTestServer struct {
	runtime  *dkdaemon.Runtime
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
}

type clientTestProxy struct {
	listener net.Listener
	ready    chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	front    net.Conn
	back     net.Conn
	dropOnce sync.Once
}

func startClientTestProxy(t *testing.T, path, backend string) *clientTestProxy {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen for client proxy: %v", err)
	}
	proxy := &clientTestProxy{listener: listener, ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(proxy.done)
		front, acceptErr := listener.Accept()
		if acceptErr != nil {
			close(proxy.ready)
			return
		}
		back, dialErr := net.Dial("unix", backend)
		if dialErr != nil {
			_ = front.Close()
			close(proxy.ready)
			return
		}
		proxy.mu.Lock()
		proxy.front = front
		proxy.back = back
		proxy.mu.Unlock()
		close(proxy.ready)

		copies := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(front, back); copies <- struct{}{} }()
		go func() { _, _ = io.Copy(back, front); copies <- struct{}{} }()
		<-copies
		_ = front.Close()
		_ = back.Close()
		<-copies
	}()
	t.Cleanup(func() { proxy.drop() })
	return proxy
}

func (p *clientTestProxy) drop() {
	p.dropOnce.Do(func() {
		_ = p.listener.Close()
		<-p.ready
		p.mu.Lock()
		if p.front != nil {
			_ = p.front.Close()
		}
		if p.back != nil {
			_ = p.back.Close()
		}
		p.mu.Unlock()
		<-p.done
	})
}

func newClientTestSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cc-interact-client-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "daemon.sock")
}

func startClientTestServer(
	t *testing.T,
	path string,
	handler wire.Handler,
	admit func() (func(), error),
) *clientTestServer {
	t.Helper()
	server := &wire.Server{WireBuild: WireBuild}
	server.Register(wire.HandlerSpec{Op: "test", Handler: handler})
	if admit != nil {
		t.Fatal("custom admission is no longer part of the product transport contract")
	}
	generation, err := proc.ProcessGeneration()
	if err != nil {
		t.Fatal(err)
	}
	reaper := &proc.Reaper{Store: &proc.FileStore{Path: filepath.Join(t.TempDir(), "processes.db")}, Generation: generation}
	children, err := proc.NewManager(4, reaper)
	if err != nil {
		t.Fatal(err)
	}
	disposable, err := worker.NewPool(worker.Config{
		Capacity: 1, QueueCapacity: 0, MaxTotalRun: 30 * time.Second,
		MaxStdinBytes: 1, MaxStdoutBytes: 1, MaxStderrBytes: 1,
	}, reaper)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := wire.NewRuntime(wire.RuntimeConfig{
		Socket: path, RuntimeBuild: "test", RuntimeProtocol: 1, Wire: server,
		TrustPolicy: testTrustPolicy(t), StopControlStore: &proc.FileStore{Path: filepath.Join(t.TempDir(), "stop.db")},
		Workers: disposable, Children: children, ShutdownTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	slot := dkdaemon.NewPublicationSlot[struct{}](runtime)
	activation, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := slot.Stage(activation, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if err := activation.CommitReady(publication); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Wait(context.Background()) }()
	running := &clientTestServer{runtime: runtime, cancel: cancel, done: done}
	t.Cleanup(func() { running.stop(t) })
	return running
}

func (s *clientTestServer) stop(t *testing.T) {
	t.Helper()
	s.stopOnce.Do(func() {
		s.cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		_ = s.runtime.Shutdown(shutdownCtx)
		select {
		case err := <-s.done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
}

func newTestClient(t *testing.T, path string, maxFrame int) *Client {
	t.Helper()
	client, err := NewClient(context.Background(), ClientConfig{
		Socket: path, WireBuild: WireBuild, Role: trust.UnprotectedRole, MaxFrameBytes: maxFrame,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return client
}

func TestClientPreSendFailureReconnectsNextOperationWithoutReplay(t *testing.T) {
	path := newClientTestSocket(t)
	var calls atomic.Int32
	startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		calls.Add(1)
		return Reply{OK: true}, nil
	}, nil)
	client := newTestClient(t, path, 256)

	_, err := client.Do(context.Background(), Envelope{
		Op: "test", Body: []byte(`{"value":"` + strings.Repeat("x", 512) + `"}`),
	})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Outcome != wire.PreSendFailure {
		t.Fatalf("oversize error = %v, want typed pre-send failure", err)
	}
	if !errors.Is(err, wire.ErrFrameTooLarge) {
		t.Fatalf("oversize error = %v, want ErrFrameTooLarge", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("oversize dispatches = %d, want 0", calls.Load())
	}

	reply, err := client.Do(context.Background(), Envelope{Op: "test"})
	if err != nil || !reply.OK {
		t.Fatalf("next operation reply=%+v err=%v", reply, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("dispatches = %d, want only the next operation", calls.Load())
	}
	if client.generation != 2 {
		t.Fatalf("session generation = %d, want 2", client.generation)
	}
}

func TestClientPostSendFailureReconnectsNextOperationWithoutReplay(t *testing.T) {
	backendPath := newClientTestSocket(t)
	path := filepath.Join(filepath.Dir(backendPath), "proxy.sock")
	entered := make(chan struct{})
	var firstCalls atomic.Int32
	first := startClientTestServer(t, backendPath, func(ctx context.Context, _ wire.Request) (any, error) {
		firstCalls.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	proxy := startClientTestProxy(t, path, backendPath)
	client := newTestClient(t, path, 0)

	result := make(chan error, 1)
	go func() {
		_, err := client.Do(context.Background(), Envelope{Op: "test"})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("operation was not dispatched")
	}
	proxy.drop()
	var err error
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not settle after disconnect")
	}
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Outcome != wire.PostSendFailure {
		t.Fatalf("error = %v, want typed post-send failure", err)
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("uncertain operation dispatches = %d, want 1", firstCalls.Load())
	}
	first.stop(t)

	var nextCalls atomic.Int32
	startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		nextCalls.Add(1)
		return Reply{OK: true}, nil
	}, nil)
	reply, err := client.Do(context.Background(), Envelope{Op: "test"})
	if err != nil || !reply.OK {
		t.Fatalf("next operation reply=%+v err=%v", reply, err)
	}
	if firstCalls.Load() != 1 || nextCalls.Load() != 1 {
		t.Fatalf("first calls=%d next calls=%d, want 1 and 1", firstCalls.Load(), nextCalls.Load())
	}
	if client.generation != 2 {
		t.Fatalf("session generation = %d, want 2", client.generation)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestClientCloseReplaysStrictTerminal(t *testing.T) {
	path := newClientTestSocket(t)
	server := startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		return Reply{OK: true}, nil
	}, nil)
	client, err := NewClient(context.Background(), ClientConfig{Socket: path, WireBuild: WireBuild, Role: trust.UnprotectedRole})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	server.stop(t)
	first := client.Close()
	if first == nil {
		t.Fatal("Close after peer exit succeeded")
	}
	second := client.Close()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second Close = %v, want replay of %v", second, first)
	}
}

func TestClientExposesBusinessOperationsOnly(t *testing.T) {
	typeOf := reflect.TypeOf((*Client)(nil))
	want := map[string]bool{"Close": true, "Do": true, "RuntimeHealth": true}
	if typeOf.NumMethod() != len(want) {
		t.Fatalf("Client exports %d methods, want %d", typeOf.NumMethod(), len(want))
	}
	for index := range typeOf.NumMethod() {
		method := typeOf.Method(index).Name
		if !want[method] {
			t.Fatalf("Client exposes unexpected method %s", method)
		}
	}
}

func TestClientBusinessSessionIsNeverProtected(t *testing.T) {
	path := newClientTestSocket(t)
	request := make(chan wire.Request, 1)
	startClientTestServer(t, path, func(_ context.Context, req wire.Request) (any, error) {
		request <- req
		return Reply{OK: true}, nil
	}, nil)
	client := newTestClient(t, path, 0)
	if _, err := client.Do(context.Background(), Envelope{Op: "test"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	req := <-request
	if req.WireBuild != WireBuild || req.Session == nil || req.Session.Protected() {
		t.Fatalf("request wire build=%q session=%v", req.WireBuild, req.Session)
	}
}

func TestClientCloseWaitsForInFlightRedial(t *testing.T) {
	path := newClientTestSocket(t)
	server := startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		return Reply{OK: true}, nil
	}, nil)
	client, err := NewClient(context.Background(), ClientConfig{Socket: path, WireBuild: WireBuild, Role: trust.UnprotectedRole})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	server.stop(t)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
	_, _ = client.Do(probeCtx, Envelope{Op: "test"})
	probeCancel()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove stale socket: %v", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen for blocked redial: %v", err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		<-release
		_ = connection.Close()
	}()
	dialCtx, dialCancel := context.WithCancel(context.Background())
	dialDone := make(chan error, 1)
	go func() {
		_, err := client.Do(dialCtx, Envelope{Op: "test"})
		dialDone <- err
	}()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("redial did not reach replacement listener")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before redial settlement: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	dialCancel()
	close(release)
	if err := <-dialDone; err == nil {
		t.Fatal("cancelled redial succeeded")
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := client.Do(context.Background(), Envelope{Op: "test"}); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("post-close Do err = %v, want ErrClientClosed", err)
	}
}

func TestDecodeStrictRejectsTrailingJSON(t *testing.T) {
	var reply Reply
	if err := decodeStrict([]byte(`{"ok":true} {"ok":false}`), &reply); err == nil {
		t.Fatal("decodeStrict accepted a trailing JSON value")
	}
}

func TestRetiringOldGenerationPreservesCurrentSession(t *testing.T) {
	old := &clientSession{generation: 1}
	current := &clientSession{generation: 2}
	client := &Client{
		current: current,
		sessions: map[*clientSession]struct{}{
			old: {}, current: {},
		},
		generation: 2,
	}
	client.retire(old)
	if !old.stale {
		t.Fatal("old generation was not retired")
	}
	if client.current != current || current.stale {
		t.Fatal("old generation failure retired the current session")
	}
}
