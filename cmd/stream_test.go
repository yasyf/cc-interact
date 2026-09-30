package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
)

// TestChannelBuildsIdentityOnce pins the unresolved-subject poll's cost: a
// channel server that polls a daemon with no subject yet, then streams and
// re-resolves after a dropped stream, builds the consumer's client identity —
// whose Program construction reads and digests the whole executable — once,
// not once per poll.
func TestChannelBuildsIdentityOnce(t *testing.T) {
	var streams atomic.Int32
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if streams.Add(1) == 1 {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(sse.Close)

	const unresolvedPolls = 2
	var resolves atomic.Int32
	spec, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		if resolves.Add(1) <= unresolvedPolls {
			return daemon.Reply{OK: true}
		}
		return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: mustPort(t, sse)}
	})
	var builds atomic.Int32
	d := testDeps(spec)
	d.NewClient = func(context.Context) (*daemon.Client, error) {
		builds.Add(1)
		return daemon.NewClient(spec)
	}
	d.WindowAlive = func(int) bool { return true }
	d.ChannelTools = func(context.Context, string, string) ([]channel.Tool, string, string, error) {
		return []channel.Tool{{Name: "noop", InputSchema: map[string]any{}}}, "notifications/test/channel", "", nil
	}

	inR, inW := io.Pipe()
	cmd := ChannelCmd(d)
	cmd.SetIn(inR)
	cmd.SetOut(&safeBuffer{})
	cmd.SetErr(&bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	deadline := time.After(10 * time.Second)
	for streams.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("stream never re-attached: %d streams after %d resolves", streams.Load(), resolves.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = inW.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("channel command did not exit after stdin EOF")
	}

	if got, want := resolves.Load(), int32(unresolvedPolls+2); got < want {
		t.Fatalf("resolves = %d, want at least %d (unresolved polls, the resolving poll, a refresh)", got, want)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("identity built %d times across %d resolves, want 1", got, resolves.Load())
	}
}

// TestReuseIdentity pins reuseIdentity's scope: one wrapper builds its identity
// once however many lanes it hands out, a failed build is retried rather than
// kept, and separate wrappers never share an identity.
func TestReuseIdentity(t *testing.T) {
	errBuild := errors.New("build failed")
	cases := []struct {
		name       string
		failures   int32
		connectors int
		calls      int
		wantBuilds int32
		wantErrs   int
	}{
		{name: "one connector builds once", connectors: 1, calls: 3, wantBuilds: 1},
		{name: "a failed build is retried", failures: 2, connectors: 1, calls: 4, wantBuilds: 3, wantErrs: 2},
		{name: "each connector builds its own", connectors: 2, calls: 3, wantBuilds: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec("cci-cmd-identity")
			var builds atomic.Int32
			build := func(context.Context) (*daemon.Client, error) {
				if builds.Add(1) <= tc.failures {
					return nil, errBuild
				}
				return daemon.NewClient(spec)
			}
			errs := 0
			for range tc.connectors {
				connect := reuseIdentity(build)
				var closed *daemon.Client
				for range tc.calls {
					lane, err := connect(context.Background())
					if errors.Is(err, errBuild) {
						errs++
						continue
					}
					if err != nil {
						t.Fatalf("connect: %v", err)
					}
					if lane == closed {
						t.Fatal("connect handed out the lane its caller already closed")
					}
					if err := lane.Close(); err != nil {
						t.Fatalf("close lane: %v", err)
					}
					closed = lane
				}
			}
			if got := builds.Load(); got != tc.wantBuilds {
				t.Fatalf("builds = %d, want %d", got, tc.wantBuilds)
			}
			if errs != tc.wantErrs {
				t.Fatalf("build errors = %d, want %d", errs, tc.wantErrs)
			}
		})
	}
}
