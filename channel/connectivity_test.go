package channel

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/trust"
)

func newDaemon(t *testing.T) *daemon.Server {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cc-interact-channel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	roles := daemon.Roles{
		Business: trust.UnprotectedRole, Lifecycle: "com.yasyf.cc-interact.channel-test.lifecycle.v1",
		StopControl: "com.yasyf.cc-interact.channel-test.stop.v1",
	}
	policy, err := trust.NewTrustPolicy(trust.TrustPolicyConfig{
		ExpectedUID: os.Geteuid(), AllowUnprotected: true,
		Roles: map[trust.PeerRole]trust.Requirement{
			roles.Lifecycle:   {TeamID: "TESTTEAM", SigningIdentifier: "com.yasyf.cc-interact.channel-test.lifecycle"},
			roles.StopControl: {TeamID: "TESTTEAM", SigningIdentifier: "com.yasyf.cc-interact.channel-test.stop"},
		},
		StopRoles: []trust.PeerRole{roles.StopControl}, ReceiptRoles: []trust.PeerRole{roles.Lifecycle},
		ReadinessRoles: []trust.PeerRole{roles.Lifecycle},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := daemon.New(daemon.Config{
		AppName:        "cc-test",
		Paths:          paths.Paths{App: ".cc-interact-test"},
		WireBuild:      daemon.WireBuild,
		RuntimeBuild:   "v1.0.0",
		TrustPolicy:    policy,
		Roles:          roles,
		ActiveStatuses: []string{"open"},
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon Serve: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		client, connectErr := daemon.NewClient(probeCtx, daemon.ClientConfig{
			Socket:    paths.Paths{App: ".cc-interact-test"}.SocketPath(),
			WireBuild: daemon.WireBuild,
			Role:      trust.UnprotectedRole,
		})
		if connectErr == nil {
			health, healthErr := client.RuntimeHealth(probeCtx)
			_ = client.Close()
			probeCancel()
			if healthErr == nil && health.Ready {
				break
			}
			connectErr = healthErr
		}
		probeCancel()
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon did not become ready: %v", connectErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s
}

func seedSubject(t *testing.T, s *daemon.Server, id string) {
	t.Helper()
	if _, err := store.NewSubjectStore(s.DB()).
		Create(context.Background(), id, id+"-slug", id+"-sess", id+"-scope", 0, "open"); err != nil {
		t.Fatalf("seed subject: %v", err)
	}
}

func presenceEvents(t *testing.T, s *daemon.Server, subjectID, typ string) []event.Event {
	t.Helper()
	evs, err := s.EventsSince(context.Background(), subjectID, 0, "")
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	var out []event.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestConnectivityBootReconcile(t *testing.T) {
	ctx := context.Background()
	s := newDaemon(t)
	seedSubject(t, s, "s1")
	c := Connectivity{}

	// A clean log has nothing to reconcile.
	if err := c.BootReconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := len(presenceEvents(t, s, "s1", DefaultConnectivityEventType)); got != 0 {
		t.Fatalf("reconcile on a clean log emitted %d events", got)
	}

	// A daemon death leaves connected:true as the log's last word.
	c.OnPresenceChange(ctx, s, "s1", true)

	if err := c.BootReconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	evs := presenceEvents(t, s, "s1", DefaultConnectivityEventType)
	if len(evs) != 2 {
		t.Fatalf("events = %d, want the stale true plus the boot false", len(evs))
	}
	closing := evs[1]
	if closing.Origin != event.OriginSystem {
		t.Fatalf("closing origin = %q, want %q", closing.Origin, event.OriginSystem)
	}
	var p struct {
		Type      string `json:"type"`
		Connected bool   `json:"connected"`
	}
	if err := json.Unmarshal(closing.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Connected || p.Type != DefaultConnectivityEventType {
		t.Fatalf("closing payload = %+v, want connected:false type %q", p, DefaultConnectivityEventType)
	}

	// Idempotent: a log already closed with connected:false is left alone.
	if err := c.BootReconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := len(presenceEvents(t, s, "s1", DefaultConnectivityEventType)); got != 2 {
		t.Fatalf("second reconcile grew the log to %d events", got)
	}
}

func TestConnectivityCustomEventType(t *testing.T) {
	ctx := context.Background()
	s := newDaemon(t)
	seedSubject(t, s, "s1")
	c := Connectivity{EventType: "presence.changed"}

	c.OnPresenceChange(ctx, s, "s1", true)
	if err := c.BootReconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := len(presenceEvents(t, s, "s1", "presence.changed")); got != 2 {
		t.Fatalf("events = %d, want 2 presence.changed", got)
	}
	if got := len(presenceEvents(t, s, "s1", DefaultConnectivityEventType)); got != 0 {
		t.Fatalf("default-type events = %d, want 0 (custom type only)", got)
	}
}

func TestConnectivityDisconnectIsNotReconciled(t *testing.T) {
	ctx := context.Background()
	s := newDaemon(t)
	seedSubject(t, s, "s1")
	c := Connectivity{}

	c.OnPresenceChange(ctx, s, "s1", true)
	c.OnPresenceChange(ctx, s, "s1", false)

	if err := c.BootReconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	// Latest is connected:false, so reconcile adds nothing.
	if got := len(presenceEvents(t, s, "s1", DefaultConnectivityEventType)); got != 2 {
		t.Fatalf("events = %d, want the two presence flips and no boot close", got)
	}
}
