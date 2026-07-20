package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/wire"
)

type allowProtectedClientTestSession struct{}

func (allowProtectedClientTestSession) Validate() error { return nil }
func (allowProtectedClientTestSession) Classify(context.Context, wire.Peer) (bool, error) {
	return true, nil
}
func (allowProtectedClientTestSession) AuthorizeBuild(string, string) bool { return true }

type blockingClientTestLifecycle struct {
	method  string
	entered chan struct{}
	release <-chan struct{}
}

func (l blockingClientTestLifecycle) Health(context.Context) (dkdaemon.Health, error) {
	if l.method == "health" {
		close(l.entered)
		<-l.release
	}
	return dkdaemon.Health{Build: "client-test", Protocol: int(wire.ProtocolVersion)}, nil
}

func (l blockingClientTestLifecycle) Shutdown(context.Context) error {
	if l.method == "shutdown" {
		close(l.entered)
		<-l.release
	}
	return nil
}

func (blockingClientTestLifecycle) Handoff(context.Context) error { return nil }

type clientTestServer struct {
	server   *wire.Server
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
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
	return startClientTestServerWithSetup(t, path, handler, admit, nil)
}

func startClientTestServerWithSetup(
	t *testing.T,
	path string,
	handler wire.Handler,
	admit func() (func(), error),
	setup func(*wire.Server),
) *clientTestServer {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove stale socket: %v", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &wire.Server{Build: "client-test"}
	server.RegisterControl("test", handler)
	if setup != nil {
		setup(server)
	}
	if admit == nil {
		admit = func() (func(), error) { return func() {}, nil }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener, func() error { return nil }, admit, admit) }()
	running := &clientTestServer{server: server, cancel: cancel, done: done}
	t.Cleanup(func() { running.stop(t) })
	return running
}

func (s *clientTestServer) stop(t *testing.T) {
	t.Helper()
	s.stopOnce.Do(func() {
		s.cancel()
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
		Socket: path, Build: "client-test", MaxFrameBytes: maxFrame,
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

func TestClientRejectedOperationIsNotReplayed(t *testing.T) {
	path := newClientTestSocket(t)
	var admissions atomic.Int32
	first := startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		t.Fatal("rejected operation reached handler")
		return nil, nil
	}, func() (func(), error) {
		admissions.Add(1)
		return nil, errors.New("closed for replacement")
	})
	client := newTestClient(t, path, 0)

	_, err := client.Do(context.Background(), Envelope{Op: "test"})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Outcome != wire.Rejected {
		t.Fatalf("error = %v, want typed rejection", err)
	}
	if admissions.Load() != 1 {
		t.Fatalf("admissions = %d, want one attempt", admissions.Load())
	}

	first.stop(t)
	var calls atomic.Int32
	startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		calls.Add(1)
		return Reply{OK: true}, nil
	}, nil)
	reply, err := client.Do(context.Background(), Envelope{Op: "test"})
	if err != nil || !reply.OK {
		t.Fatalf("next operation reply=%+v err=%v", reply, err)
	}
	if calls.Load() != 1 || admissions.Load() != 1 {
		t.Fatalf("calls=%d rejected admissions=%d, want 1 and 1", calls.Load(), admissions.Load())
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestClientPostSendFailureReconnectsNextOperationWithoutReplay(t *testing.T) {
	path := newClientTestSocket(t)
	entered := make(chan struct{})
	var firstCalls atomic.Int32
	first := startClientTestServer(t, path, func(ctx context.Context, _ wire.Request) (any, error) {
		firstCalls.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
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
	first.stop(t)
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
	client, err := NewClient(context.Background(), ClientConfig{Socket: path, Build: "client-test"})
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

func TestClientCloseIsTerminalBarrierForLifecycleOperations(t *testing.T) {
	for _, method := range []string{"health", "shutdown"} {
		t.Run(method, func(t *testing.T) {
			path := newClientTestSocket(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			lifecycle := blockingClientTestLifecycle{method: method, entered: entered, release: release}
			startClientTestServerWithSetup(
				t,
				path,
				func(context.Context, wire.Request) (any, error) { return Reply{OK: true}, nil },
				nil,
				func(server *wire.Server) {
					server.ReservedProtectedSessions = 1
					server.ProtectedSessionClassifier = allowProtectedClientTestSession{}
					server.RegisterLifecycle(lifecycle)
				},
			)
			client, err := NewClient(context.Background(), ClientConfig{Socket: path, Build: "client-test"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			opDone := make(chan error, 1)
			go func() {
				if method == "health" {
					_, err := client.Health(context.Background())
					opDone <- err
					return
				}
				opDone <- client.Shutdown(context.Background())
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("lifecycle operation did not enter")
			}
			closeDone := make(chan error, 1)
			go func() { closeDone <- client.Close() }()
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned before lifecycle settlement: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			if err := <-opDone; err != nil {
				t.Fatalf("lifecycle operation: %v", err)
			}
			if err := <-closeDone; err != nil {
				t.Fatalf("Close: %v", err)
			}
			if _, err := client.Health(context.Background()); !errors.Is(err, ErrClientClosed) {
				t.Fatalf("post-close Health err = %v, want ErrClientClosed", err)
			}
			if err := client.Shutdown(context.Background()); !errors.Is(err, ErrClientClosed) {
				t.Fatalf("post-close Shutdown err = %v, want ErrClientClosed", err)
			}
		})
	}
}

func TestClientCloseWaitsForInFlightRedial(t *testing.T) {
	path := newClientTestSocket(t)
	server := startClientTestServer(t, path, func(context.Context, wire.Request) (any, error) {
		return Reply{OK: true}, nil
	}, nil)
	client, err := NewClient(context.Background(), ClientConfig{Socket: path, Build: "client-test"})
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
