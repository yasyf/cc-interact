package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/trust"
	"github.com/yasyf/daemonkit/wire"
)

func TestLauncherValidationAcceptsExactAgentAndRoles(t *testing.T) {
	launcher := Launcher{WireBuild: WireBuild, RuntimeBuild: "1.0.0"}
	if err := launcher.validate(); err == nil {
		t.Fatal("validate accepted missing service agent and roles")
	}
	launcher.Agent = testAgent(t)
	if err := launcher.validate(); err == nil {
		t.Fatal("validate accepted missing roles")
	}
	launcher.Roles = testRoles()
	if err := launcher.validate(); err != nil {
		t.Fatalf("validate exact launcher: %v", err)
	}
}

func TestRuntimeClientConfigCarriesExactRole(t *testing.T) {
	launcher := Launcher{Paths: testPaths(), WireBuild: WireBuild}
	config := launcher.runtimeClientConfig(testRoles().Lifecycle, time.Second)
	if config.Client.Role != testRoles().Lifecycle {
		t.Fatalf("role = %q, want %q", config.Client.Role, testRoles().Lifecycle)
	}
	if config.NoProgressTimeout != time.Second {
		t.Fatalf("no-progress timeout = %s", config.NoProgressTimeout)
	}
}

func TestRuntimeActionBranches(t *testing.T) {
	launcher := Launcher{RuntimeBuild: "1.0.0"}
	ready := RuntimeHealth{
		RuntimeBuild: "1.0.0", RuntimeProtocol: int(wire.ProtocolVersion), ProcessGeneration: "generation-1",
		Ready: true, State: dkdaemon.StateHealthy,
	}
	tests := []struct {
		name    string
		health  RuntimeHealth
		want    runtimeAction
		wantErr bool
	}{
		{name: "exact ready healthy", health: ready, want: runtimeNoop},
		{name: "newer refuses", health: func() RuntimeHealth { h := ready; h.RuntimeBuild = "2.0.0"; return h }(), wantErr: true},
		{name: "older upgrades", health: func() RuntimeHealth { h := ready; h.RuntimeBuild = "0.9.0"; return h }(), want: runtimeStopUpgrade},
		{name: "starting observes", health: func() RuntimeHealth { h := ready; h.Ready = false; h.State = RuntimeStateStarting; return h }(), want: runtimeObserve},
		{name: "draining observes", health: func() RuntimeHealth { h := ready; h.Draining = true; return h }(), want: runtimeObserve},
		{name: "failed stops", health: func() RuntimeHealth { h := ready; h.State = dkdaemon.StateFailed; return h }(), want: runtimeStopRestart},
		{name: "busy degraded observes", health: func() RuntimeHealth { h := ready; h.State = dkdaemon.StateDegraded; h.Busy = true; return h }(), want: runtimeObserve},
		{name: "idle degraded stops", health: func() RuntimeHealth { h := ready; h.State = dkdaemon.StateDegraded; return h }(), want: runtimeStopRestart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := launcher.runtimeAction(tt.health)
			if (err != nil) != tt.wantErr {
				t.Fatalf("runtimeAction error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("runtimeAction = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSettleRuntimeBranches(t *testing.T) {
	launcher := Launcher{RuntimeBuild: "1.0.0"}
	ready := RuntimeHealth{
		RuntimeBuild: "1.0.0", RuntimeProtocol: int(wire.ProtocolVersion), ProcessGeneration: "generation-1",
		Ready: true, State: dkdaemon.StateHealthy,
	}
	starting := ready
	starting.Ready = false
	starting.State = RuntimeStateStarting
	_, action, err := launcher.settleRuntime(t.Context(), time.Second, starting, func(context.Context) (RuntimeHealth, error) {
		return ready, nil
	})
	if err != nil || action != runtimeNoop {
		t.Fatalf("settle ready = %d, %v", action, err)
	}
	draining := ready
	draining.Draining = true
	_, action, err = launcher.settleRuntime(t.Context(), time.Second, draining, func(context.Context) (RuntimeHealth, error) {
		return RuntimeHealth{}, fmt.Errorf("gone: %w", ErrNoPeer)
	})
	if err != nil || action != runtimeSpawn {
		t.Fatalf("settle gone = %d, %v", action, err)
	}
}

func TestRolesRejectMissingAuthority(t *testing.T) {
	roles := testRoles()
	roles.Business = trust.PeerRole("missing")
	if err := roles.validate(testTrustPolicy(t)); err == nil {
		t.Fatal("roles accepted an undeclared business authority")
	}
}
