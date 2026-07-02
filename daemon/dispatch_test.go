package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/paths"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
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
	if cfg.ActiveStatuses == nil {
		cfg.ActiveStatuses = []string{"open"}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.store.Close() })
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

func TestDispatchHealthReportsVersion(t *testing.T) {
	s := newTestServer(t, Config{Version: "v2.3.4"})
	r := s.dispatch(context.Background(), Envelope{Op: OpHealth})
	if !r.OK || r.DaemonVersion != "v2.3.4" {
		t.Fatalf("health = %+v, want ok with version v2.3.4", r)
	}
}

func TestDispatchProtoSkew(t *testing.T) {
	s := newTestServer(t, Config{})
	r := s.dispatch(context.Background(), Envelope{Proto: 999, Op: OpResolve, Scope: "x"})
	if r.OK || !contains(r.Error, "protocol skew") {
		t.Fatalf("skew dispatch = %+v, want protocol-skew error", r)
	}
}

func TestRegisterPanicsOnReservedOp(t *testing.T) {
	s := newTestServer(t, Config{})
	defer func() {
		if recover() == nil {
			t.Fatal("Register of a reserved core op must panic")
		}
	}()
	s.Register(OpHealth, func(HandlerCtx) Reply { return Reply{} })
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

func TestDispatchRoutesRegisteredOp(t *testing.T) {
	s := newTestServer(t, Config{
		ScopeResolve: func(_ context.Context, raw string) (string, error) { return raw + "!", nil },
	})
	var gotScope string
	s.Register("custom", func(hc HandlerCtx) Reply {
		gotScope = hc.Scope
		return Reply{OK: true, Body: json.RawMessage(`{"hi":1}`)}
	})
	r := s.dispatch(context.Background(), Envelope{Proto: ProtocolVersion, Op: "custom", Scope: "x"})
	if !r.OK || string(r.Body) != `{"hi":1}` {
		t.Fatalf("custom dispatch = %+v, want body passthrough", r)
	}
	if gotScope != "x!" {
		t.Fatalf("handler saw scope %q, want resolved x!", gotScope)
	}
}

func TestDispatchUnknownOp(t *testing.T) {
	s := newTestServer(t, Config{})
	r := s.dispatch(context.Background(), Envelope{Proto: ProtocolVersion, Op: "nope", Scope: "x"})
	if r.OK || !contains(r.Error, "unknown op") {
		t.Fatalf("unknown op = %+v, want unknown-op error", r)
	}
}

func TestDispatchScopeResolveErrorPerOp(t *testing.T) {
	s := newTestServer(t, Config{
		ScopeResolve: func(context.Context, string) (string, error) { return "", errors.New("not a scope") },
	})
	ctx := context.Background()

	if r := s.dispatch(ctx, Envelope{Proto: ProtocolVersion, Op: OpGuardEdit, Scope: "x"}); !r.OK || !r.Allow {
		t.Fatalf("guard-edit on scope error = %+v, want allow (nothing to guard)", r)
	}
	if r := s.dispatch(ctx, Envelope{Proto: ProtocolVersion, Op: OpSessionRecord, Scope: "x", Session: "s"}); !r.OK {
		t.Fatalf("session-record on scope error = %+v, want ok no-op", r)
	}
	if r := s.dispatch(ctx, Envelope{Proto: ProtocolVersion, Op: OpStatus, Scope: "x"}); !r.OK || r.DaemonVersion == "" {
		t.Fatalf("status on scope error = %+v, want ok with daemon version", r)
	}
	if r := s.dispatch(ctx, Envelope{Proto: ProtocolVersion, Op: OpResolve, Scope: "x"}); r.OK || !contains(r.Error, "not a scope") {
		t.Fatalf("resolve on scope error = %+v, want error", r)
	}
}

func TestDispatchResolveFindsSubject(t *testing.T) {
	s := newTestServer(t, Config{})
	seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 4242, "open")
	r := s.dispatch(context.Background(), Envelope{
		Proto: ProtocolVersion, Op: OpResolve, Scope: "scopeA", Session: "sess1", ClaudePID: 4242, Consumer: "channel",
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
	if r := s.dispatch(context.Background(), Envelope{Proto: ProtocolVersion, Op: OpChannelAck}); r.OK {
		t.Fatalf("channel-ack without a pid = %+v, want error", r)
	}
	if r := s.dispatch(context.Background(), Envelope{Proto: ProtocolVersion, Op: OpChannelAck, ClaudePID: 77}); !r.OK {
		t.Fatalf("channel-ack with a pid = %+v, want ok", r)
	}
	if !s.activity.Proven(77) {
		t.Fatal("channel-ack must mark the window proven")
	}
}

func TestDispatchStatusReportsSubject(t *testing.T) {
	s := newTestServer(t, Config{})
	seedSubject(t, s, "id2", "slug2", "sess2", "scopeB", 9, "open")
	r := s.dispatch(context.Background(), Envelope{
		Proto: ProtocolVersion, Op: OpStatus, Scope: "scopeB", Session: "sess2", ClaudePID: 9,
	})
	if !r.OK || r.SubjectID != "id2" || r.Status != "open" || r.DaemonVersion == "" {
		t.Fatalf("status = %+v, want id2/open with daemon version", r)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
