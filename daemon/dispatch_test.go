package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/paths"
)

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if cfg.AppName == "" {
		cfg.AppName = "cc-test"
	}
	cfg.Paths = paths.Paths{App: ".cc-interact-test"}
	if cfg.Version == "" {
		cfg.Version = "v1.0.0"
	}
	if cfg.LifecycleBuild == "" {
		cfg.LifecycleBuild = "v1.0.0"
	}
	if cfg.ActiveStatuses == nil {
		cfg.ActiveStatuses = []string{"open"}
	}
	if cfg.DaemonRole.RoleID == "" {
		cfg.DaemonRole = testDaemonRole(t)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.paths.EnsureStateDir(); err != nil {
		t.Fatalf("ensure state dir: %v", err)
	}
	if err := s.activate(dkdaemon.Activation{Startup: context.Background(), Lifetime: context.Background()}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	t.Cleanup(func() { _ = s.closeState() })
	return s
}

// seedSubject inserts a subject directly so dispatch can resolve it.
func seedSubject(t *testing.T, s *Server, id, slug, session, scope string, pid int, status string) subject.Subject {
	t.Helper()
	sub, err := store.NewSubjectStore(s.DB()).
		Create(context.Background(), id, slug, session, scope, pid, status)
	if err != nil {
		t.Fatalf("seed subject: %v", err)
	}
	return sub
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	s := newTestServer(t, Config{})
	s.Register("custom", func(HandlerCtx) Reply { return Reply{} })
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register must panic")
		}
	}()
	s.Register("custom", func(HandlerCtx) Reply { return Reply{} })
}

func TestRegisterPanicsOnCoreOp(t *testing.T) {
	s := newTestServer(t, Config{})
	defer func() {
		if recover() == nil {
			t.Fatal("Register of a core op wired in New must panic as a duplicate")
		}
	}()
	s.Register(OpStatus, func(HandlerCtx) Reply { return Reply{} })
}

func TestRegisterPanicsAfterFreeze(t *testing.T) {
	s := newTestServer(t, Config{})
	s.freezeHandlers()
	defer func() {
		if recover() == nil {
			t.Fatal("Register after runtime freeze must panic")
		}
	}()
	s.Register("custom", func(HandlerCtx) Reply { return Reply{} })
}

func TestDecodeEnvelopeRejectsTrailingAndUnknownJSON(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"scope":"/repo"} {"scope":"/other"}`),
		[]byte(`{"scope":"/repo","unknown":true}`),
	} {
		if _, err := decodeEnvelope(payload); err == nil {
			t.Fatalf("decodeEnvelope(%q) succeeded", payload)
		}
	}
}

func TestDispatchRoutesRegisteredOp(t *testing.T) {
	s := newTestServer(t, Config{
		ScopeResolve: func(_ context.Context, raw string) string { return raw + "!" },
	})
	var gotScope string
	s.Register("custom", func(hc HandlerCtx) Reply {
		gotScope = hc.Scope
		return Reply{OK: true, Body: json.RawMessage(`{"hi":1}`)}
	})
	r := s.dispatch(context.Background(), Envelope{Op: "custom", Scope: "x"})
	if !r.OK || string(r.Body) != `{"hi":1}` {
		t.Fatalf("custom dispatch = %+v, want body passthrough", r)
	}
	if gotScope != "x!" {
		t.Fatalf("handler saw scope %q, want resolved x!", gotScope)
	}
}

func TestDispatchUnknownOp(t *testing.T) {
	s := newTestServer(t, Config{})
	r := s.dispatch(context.Background(), Envelope{Op: "nope", Scope: "x"})
	if r.OK || !contains(r.Error, "unknown op") {
		t.Fatalf("unknown op = %+v, want unknown-op error", r)
	}
}

func TestDispatchFallbackScopeDegradesCoreOps(t *testing.T) {
	s := newTestServer(t, Config{
		ScopeResolve: func(_ context.Context, raw string) string { return raw },
	})
	ctx := context.Background()

	if r := s.dispatch(ctx, Envelope{Op: OpGuardEdit, Scope: "/not/a/repo"}); !r.OK || !r.Allow {
		t.Fatalf("guard-edit on fallback scope = %+v, want allow (nothing to guard)", r)
	}
	if r := s.dispatch(ctx, Envelope{Op: OpSessionRecord, Scope: "/not/a/repo", Session: "s"}); !r.OK {
		t.Fatalf("session-record on fallback scope = %+v, want ok no-op", r)
	}
	if r := s.dispatch(ctx, Envelope{Op: OpStatus, Scope: "/not/a/repo"}); !r.OK || r.DaemonVersion == "" || r.SubjectID != "" {
		t.Fatalf("status on fallback scope = %+v, want liveness only", r)
	}
	// Pre-0.1.9 resolve errored here; a fallback scope now matches no subject.
	if r := s.dispatch(ctx, Envelope{Op: OpResolve, Scope: "/not/a/repo"}); !r.OK || r.SubjectID != "" {
		t.Fatalf("resolve on fallback scope = %+v, want ok with no subject", r)
	}
}

func TestDispatchResolveFindsSubject(t *testing.T) {
	s := newTestServer(t, Config{})
	seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 4242, "open")
	r := s.dispatch(context.Background(), Envelope{
		Op: OpResolve, Scope: "scopeA", Session: "sess1", ClaudePID: 4242, Consumer: "channel",
	})
	if !r.OK || r.SubjectID != "id1" || r.Status != "open" {
		t.Fatalf("resolve = %+v, want id1/open", r)
	}
	if !s.activity.PolledSince("scopeA", "channel", 4242, time.Hour) {
		t.Fatal("resolve with a consumer must record a poll")
	}
}

func TestDispatchChannelAck(t *testing.T) {
	s := newTestServer(t, Config{})
	if r := s.dispatch(context.Background(), Envelope{Op: OpChannelAck}); r.OK {
		t.Fatalf("channel-ack without a pid = %+v, want error", r)
	}
	if r := s.dispatch(context.Background(), Envelope{Op: OpChannelAck, ClaudePID: 77}); !r.OK {
		t.Fatalf("channel-ack with a pid = %+v, want ok", r)
	}
	if !s.activity.Proven(77) {
		t.Fatal("channel-ack must mark the window proven")
	}
}

func TestDispatchStatusReportsSubject(t *testing.T) {
	s := newTestServer(t, Config{})
	seedSubject(t, s, "id2", "slug2", "sess2", "scopeB", 9, "open")
	s.activity.Attach("id2", "watch-100", 100)
	s.activity.Attach("id2", "watch-100", 200)
	s.activity.Attach("id2", "channel", 100)
	r := s.dispatch(context.Background(), Envelope{
		Op: OpStatus, Scope: "scopeB", Session: "sess2", ClaudePID: 9,
	})
	if !r.OK || r.SubjectID != "id2" || r.Status != "open" || r.DaemonVersion == "" {
		t.Fatalf("status = %+v, want id2/open with daemon version", r)
	}
	var body StatusBody
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("unmarshal status body: %v", err)
	}
	if !body.ConsumerConnected || !maps.Equal(body.Consumers, map[string]int{"watch-100": 2, "channel": 1}) {
		t.Fatalf("status body = %+v, want connected consumers", body)
	}
}

func TestDispatchPublic(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register bool
		env      Envelope
		wantOK   bool
		wantErr  string
		wantBody string
	}{
		{
			name:     "registered op round-trips",
			register: true,
			env:      Envelope{Op: "echo", Body: json.RawMessage(`{"msg":"hi"}`)},
			wantOK:   true,
			wantBody: `{"echo":"hi"}`,
		},
		{
			name:    "unknown op errors",
			env:     Envelope{Op: "nope"},
			wantOK:  false,
			wantErr: "nope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, Config{})
			if tc.register {
				s.Register("echo", func(hc HandlerCtx) Reply {
					var body struct {
						Msg string `json:"msg"`
					}
					if err := json.Unmarshal(hc.Env.Body, &body); err != nil {
						t.Fatalf("unmarshal envelope body: %v", err)
					}
					return Reply{OK: true, Body: json.RawMessage(fmt.Sprintf(`{"echo":%q}`, body.Msg))}
				})
			}
			r := s.Dispatch(context.Background(), tc.env)
			if r.OK != tc.wantOK {
				t.Fatalf("Dispatch(%+v) ok = %v, want %v (reply: %+v)", tc.env, r.OK, tc.wantOK, r)
			}
			if tc.wantErr != "" && !contains(r.Error, tc.wantErr) {
				t.Fatalf("Dispatch error = %q, want substring %q", r.Error, tc.wantErr)
			}
			if tc.wantBody != "" && string(r.Body) != tc.wantBody {
				t.Fatalf("Dispatch body = %s, want %s", r.Body, tc.wantBody)
			}
		})
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
