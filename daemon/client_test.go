package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/daemonkit"
)

type stubProduct struct {
	handle func(context.Context, daemonkit.Request) (daemonkit.Reply, error)
}

func (p stubProduct) Handle(ctx context.Context, req daemonkit.Request) (daemonkit.Reply, error) {
	return p.handle(ctx, req)
}

func (stubProduct) Drain(daemonkit.Budget) error { return nil }

func (stubProduct) Close(daemonkit.Budget) error { return nil }

// serveStub runs a daemonkit daemon in-process whose product is handle, and
// returns the shared spec once the business lane dispatches.
func serveStub(t *testing.T, handle func(context.Context, daemonkit.Request) (daemonkit.Reply, error)) daemonkit.Daemon {
	t.Helper()
	spec, _ := serveStubDaemon(t, "cci-client-test", handle)
	return spec
}

// serveStubDaemon runs the stub daemon under label and returns the shared spec
// plus the stop that takes the daemon down mid-test. The cleanup runs the same
// stop.
func serveStubDaemon(
	t *testing.T,
	label string,
	handle func(context.Context, daemonkit.Request) (daemonkit.Reply, error),
) (daemonkit.Daemon, func()) {
	t.Helper()
	shortHome(t)
	spec := Spec(daemonkit.Daemon{
		Label: daemonkit.Label(label),
		Trust: daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := daemonkit.Serve(ctx, spec, func(daemonkit.Ctx) (daemonkit.Product, error) {
			return stubProduct{handle: handle}, nil
		})
		done <- err
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stub Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("stub daemon did not stop")
		}
	})
	t.Cleanup(stop)
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, probeErr := client.Do(probeCtx, Envelope{Op: "probe"})
		probeCancel()
		var productErr *daemonkit.ProductError
		if probeErr == nil || errors.Is(probeErr, ErrMalformedReply) || errors.As(probeErr, &productErr) {
			return spec, stop
		}
		if time.Now().After(deadline) {
			t.Fatalf("stub daemon did not become ready: %v", probeErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func replyWith(reply Reply) func(context.Context, daemonkit.Request) (daemonkit.Reply, error) {
	return func(context.Context, daemonkit.Request) (daemonkit.Reply, error) {
		body, err := json.Marshal(reply)
		if err != nil {
			return daemonkit.Reply{}, err
		}
		return daemonkit.Reply{Body: body}, nil
	}
}

func TestDoRoundTripsEnvelopeAndReply(t *testing.T) {
	received := make(chan Envelope, 1)
	spec := serveStub(t, func(_ context.Context, req daemonkit.Request) (daemonkit.Reply, error) {
		if Op(req.Op) == "echo" {
			var env Envelope
			if err := json.Unmarshal(req.Body, &env); err != nil {
				return daemonkit.Reply{}, err
			}
			env.Op = Op(req.Op)
			received <- env
			return daemonkit.Reply{Body: []byte(`{"ok":true,"subject_id":"sub-1","http_port":4321}`)}, nil
		}
		return daemonkit.Reply{Body: []byte(`{"ok":true}`)}, nil
	})
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	reply, err := client.Do(context.Background(), Envelope{Op: "echo", Session: "s1", ClaudePID: 42, Scope: "/repo"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !reply.OK || reply.SubjectID != "sub-1" || reply.HTTPPort != 4321 {
		t.Fatalf("reply = %+v", reply)
	}
	got := <-received
	if got.Op != "echo" || got.Session != "s1" || got.ClaudePID != 42 || got.Scope != "/repo" {
		t.Fatalf("envelope = %+v", got)
	}
}

func TestDoInBandErrorRidesTheReply(t *testing.T) {
	spec := serveStub(t, replyWith(Reply{OK: false, Error: "no window"}))
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	reply, err := client.Do(context.Background(), Envelope{Op: "any"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if reply.OK || reply.Error != "no window" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestDoMalformedReplyIsTyped(t *testing.T) {
	spec := serveStub(t, func(context.Context, daemonkit.Request) (daemonkit.Reply, error) {
		return daemonkit.Reply{Body: []byte(`{"ok":true} trailing`)}, nil
	})
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Do(context.Background(), Envelope{Op: "any"}); !errors.Is(err, ErrMalformedReply) {
		t.Fatalf("Do error = %v, want ErrMalformedReply", err)
	}
}

func TestDoProductErrorCrossesTheWire(t *testing.T) {
	spec := serveStub(t, func(context.Context, daemonkit.Request) (daemonkit.Reply, error) {
		return daemonkit.Reply{}, &daemonkit.ProductError{Code: "cc.bad", Message: "broken"}
	})
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	_, err = client.Do(context.Background(), Envelope{Op: "any"})
	var productErr *daemonkit.ProductError
	if !errors.As(err, &productErr) || productErr.Code != "cc.bad" {
		t.Fatalf("Do error = %v, want ProductError cc.bad", err)
	}
}

func TestDoCarriesAWholeWritePayload(t *testing.T) {
	spec := serveStub(t, replyWith(Reply{OK: true}))
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	overhead, err := json.Marshal(Envelope{Op: OpGuardEdit, Body: json.RawMessage(`""`)})
	if err != nil {
		t.Fatal(err)
	}
	filler := bytes.Repeat([]byte("x"), maxPayloadBytes-len(overhead))
	body := append(append([]byte(`"`), filler...), '"')
	env := Envelope{Op: OpGuardEdit, Body: body}
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != maxPayloadBytes {
		t.Fatalf("payload = %d bytes, want exactly %d", len(payload), maxPayloadBytes)
	}
	reply, err := client.Do(context.Background(), env)
	if err != nil {
		t.Fatalf("Do at the declared payload ceiling: %v", err)
	}
	if !reply.OK {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestClientCloseReplaysStrictTerminal(t *testing.T) {
	spec, stop := serveStubDaemon(t, "cci-client-close", replyWith(Reply{OK: true}))
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Do(context.Background(), Envelope{Op: "any"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	stop()
	first := client.Close()
	if first == nil {
		t.Fatal("Close after the peer left succeeded")
	}
	if second := client.Close(); second == nil || second.Error() != first.Error() {
		t.Fatalf("second Close = %v, want a replay of %v", second, first)
	}
}

func TestDoAbsentDaemonIsProvenAbsent(t *testing.T) {
	shortHome(t)
	spec := Spec(daemonkit.Daemon{
		Label: "cci-client-absent",
		Trust: daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	})
	client, err := NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Do(context.Background(), Envelope{Op: "any"}); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("Do error = %v, want ErrNoPeer", err)
	}
}
