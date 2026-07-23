package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/daemonrole"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/wire"
)

const (
	controlHelperEnv  = "CC_INTERACT_CONTROL_HELPER"
	stopExecHelperEnv = "CC_INTERACT_STOP_EXEC_HELPER"
)

func TestLauncherControlHelper(t *testing.T) {
	if os.Getenv(controlHelperEnv) != "1" {
		return
	}
	p := paths.Paths{App: os.Getenv("CC_INTERACT_APP")}
	if err := p.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		AppName: "control-helper", Paths: p,
		WireBuild:    os.Getenv("CC_INTERACT_WIRE_BUILD"),
		RuntimeBuild: os.Getenv("CC_INTERACT_RUNTIME_BUILD"),
		DaemonRole: daemonrole.Classifier{
			RoleID: os.Getenv("CC_INTERACT_ROLE_ID"), RolePath: os.Getenv("CC_INTERACT_ROLE_PATH"),
		},
		ActiveStatuses: []string{"open"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLauncherStopExecHelper(t *testing.T) {
	if os.Getenv(stopExecHelperEnv) != "1" {
		return
	}
	payload, err := json.Marshal(os.Args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("CC_INTERACT_STOP_ARGS_FILE"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := Launcher{
		Paths:        paths.Paths{App: os.Getenv("CC_INTERACT_APP")},
		WireBuild:    os.Getenv("CC_INTERACT_WIRE_BUILD"),
		RuntimeBuild: os.Getenv("CC_INTERACT_RUNTIME_BUILD"),
		Args:         []string{"daemon"}, StopArgs: []string{StopControlCommand},
		DaemonRole: daemonrole.Classifier{
			RoleID: os.Getenv("CC_INTERACT_ROLE_ID"), RolePath: os.Getenv("CC_INTERACT_ROLE_PATH"),
		},
	}
	if err := launcher.RunStopControl(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLauncherStopDirectExecsRolePathWithExactArgs(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "cci-stop-exec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(home, "exact-role")
	if err := os.Symlink(target, rolePath); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "stop-args.json")
	untouched := filepath.Join(home, "must-not-exist")
	stopArgs := []string{"-test.run=TestLauncherStopExecHelper", "--", "literal with spaces", "$(touch " + untouched + ")"}
	const app = ".cc-interact-stop-exec"
	role := daemonrole.Classifier{RoleID: "com.yasyf.cc-interact.stop-exec", RolePath: rolePath}
	t.Setenv(stopExecHelperEnv, "1")
	t.Setenv("CC_INTERACT_STOP_ARGS_FILE", argsPath)
	t.Setenv("CC_INTERACT_APP", app)
	t.Setenv("CC_INTERACT_WIRE_BUILD", WireBuild)
	t.Setenv("CC_INTERACT_RUNTIME_BUILD", "1.0.0")
	t.Setenv("CC_INTERACT_ROLE_ID", role.RoleID)
	t.Setenv("CC_INTERACT_ROLE_PATH", role.RolePath)
	command := exec.Command(executable, "-test.run=TestLauncherControlHelper")
	command.Env = append(os.Environ(),
		controlHelperEnv+"=1", "HOME="+home, "CC_INTERACT_APP="+app,
		"CC_INTERACT_WIRE_BUILD="+WireBuild, "CC_INTERACT_RUNTIME_BUILD=1.0.0",
		"CC_INTERACT_ROLE_ID="+role.RoleID, "CC_INTERACT_ROLE_PATH="+role.RolePath,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	p := paths.Paths{App: app}
	waitForSocket(t, p.SocketPath())
	l := Launcher{
		Paths: p, WireBuild: WireBuild, RuntimeBuild: "1.0.0",
		Args: []string{"daemon"}, StopArgs: stopArgs,
		DaemonRole: role,
	}
	if err := l.Stop(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	payload, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, stopArgs) {
		t.Fatalf("control args = %#v, want %#v", got, stopArgs)
	}
	if _, err := os.Stat(untouched); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal shell syntax was executed: %v", err)
	}
}

func TestEnsureCurrentStopsOlderGenerationBeforeSpawningCurrent(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "cci-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(home, "exact-role")
	if err := os.Symlink(target, rolePath); err != nil {
		t.Fatal(err)
	}
	const app = ".cc-interact-upgrade"
	role := daemonrole.Classifier{RoleID: "com.yasyf.cc-interact.upgrade", RolePath: rolePath}
	incumbent := exec.Command(executable, "-test.run=TestLauncherControlHelper")
	incumbent.Env = append(os.Environ(),
		controlHelperEnv+"=1", "HOME="+home, "CC_INTERACT_APP="+app,
		"CC_INTERACT_WIRE_BUILD="+WireBuild, "CC_INTERACT_RUNTIME_BUILD=1.0.0",
		"CC_INTERACT_ROLE_ID="+role.RoleID, "CC_INTERACT_ROLE_PATH="+role.RolePath,
	)
	if err := incumbent.Start(); err != nil {
		t.Fatal(err)
	}
	incumbentWaited := false
	t.Cleanup(func() {
		if !incumbentWaited {
			_ = incumbent.Process.Kill()
			_ = incumbent.Wait()
		}
	})
	p := paths.Paths{App: app}
	waitForSocket(t, p.SocketPath())

	t.Setenv(controlHelperEnv, "1")
	t.Setenv(stopExecHelperEnv, "1")
	t.Setenv("CC_INTERACT_STOP_ARGS_FILE", filepath.Join(home, "stop-args.json"))
	t.Setenv("CC_INTERACT_APP", app)
	t.Setenv("CC_INTERACT_WIRE_BUILD", WireBuild)
	t.Setenv("CC_INTERACT_RUNTIME_BUILD", "2.0.0")
	t.Setenv("CC_INTERACT_ROLE_ID", role.RoleID)
	t.Setenv("CC_INTERACT_ROLE_PATH", role.RolePath)
	launcher := Launcher{
		Paths: p, WireBuild: WireBuild, RuntimeBuild: "2.0.0",
		Args:       []string{"-test.run=TestLauncherControlHelper"},
		StopArgs:   []string{"-test.run=TestLauncherStopExecHelper"},
		DaemonRole: role,
	}
	if err := launcher.EnsureCurrent(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if err := incumbent.Wait(); err != nil {
		t.Fatalf("incumbent exit: %v", err)
	}
	incumbentWaited = true
	health, err := launcher.runtimeHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.ready(health) || health.RuntimeBuild != "2.0.0" {
		t.Fatalf("successor health = %+v, want ready build 2.0.0", health)
	}
	if err := launcher.Stop(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("stop successor: %v", err)
	}
}

func TestLauncherStopReportsAbsentWithoutExecutingRole(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "cci-stop-absent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(home, "exact-role")
	if err := os.Symlink(executable, rolePath); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "stop-args.json")
	t.Setenv(stopExecHelperEnv, "1")
	t.Setenv("CC_INTERACT_STOP_ARGS_FILE", argsPath)
	l := Launcher{
		Paths: paths.Paths{App: ".cc-interact-stop-absent"}, WireBuild: WireBuild, RuntimeBuild: "1.0.0",
		Args: []string{"daemon"}, StopArgs: []string{"-test.run=TestLauncherStopExecHelper"},
		DaemonRole: daemonrole.Classifier{RoleID: "com.yasyf.cc-interact.stop-absent", RolePath: rolePath},
	}
	if err := l.Stop(context.Background(), 5*time.Second); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("Stop error = %v, want ErrNoPeer", err)
	}
	if _, err := os.Stat(argsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact role ran for an absent daemon: %v", err)
	}
}

func TestEnsureCurrentIfRunningReportsProvenAbsence(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "cci-running-absent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(home, "exact-role")
	if err := os.Symlink(executable, rolePath); err != nil {
		t.Fatal(err)
	}
	l := Launcher{
		Paths: paths.Paths{App: ".cc-interact-running-absent"}, WireBuild: WireBuild, RuntimeBuild: "1.0.0",
		Args: []string{"daemon"}, StopArgs: []string{StopControlCommand},
		DaemonRole: daemonrole.Classifier{RoleID: "com.yasyf.cc-interact.running-absent", RolePath: rolePath},
	}
	if err := l.EnsureCurrentIfRunning(context.Background()); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("EnsureCurrentIfRunning error = %v, want ErrNoPeer", err)
	}
}

func TestRuntimeHealthDoesNotMapProtocolFailureToNoPeer(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "cci-health-protocol-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	p := paths.Paths{App: ".cc-interact-health-protocol"}
	if err := p.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", p.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()
	_, err = (Launcher{Paths: p, WireBuild: WireBuild}).runtimeHealth(context.Background())
	<-accepted
	if err == nil {
		t.Fatal("runtimeHealth succeeded against an invalid wire peer")
	}
	if errors.Is(err, ErrNoPeer) {
		t.Fatalf("runtimeHealth mapped protocol failure to ErrNoPeer: %v", err)
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
		{name: "missing build refuses", health: func() RuntimeHealth { h := ready; h.RuntimeBuild = ""; return h }(), wantErr: true},
		{name: "older missing generation refuses", health: func() RuntimeHealth { h := ready; h.RuntimeBuild = "0.9.0"; h.ProcessGeneration = ""; return h }(), wantErr: true},
		{name: "older wrong protocol refuses", health: func() RuntimeHealth { h := ready; h.RuntimeBuild = "0.9.0"; h.RuntimeProtocol++; return h }(), wantErr: true},
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
	t.Run("starting publishes ready", func(t *testing.T) {
		starting := ready
		starting.Ready = false
		starting.State = RuntimeStateStarting
		_, action, err := launcher.settleRuntime(t.Context(), time.Second, starting, func(context.Context) (RuntimeHealth, error) {
			return ready, nil
		})
		if err != nil || action != runtimeNoop {
			t.Fatalf("settleRuntime = %d, %v; want no-op", action, err)
		}
	})
	t.Run("starting failure stops", func(t *testing.T) {
		starting := ready
		starting.Ready = false
		starting.State = RuntimeStateStarting
		failed := starting
		failed.State = dkdaemon.StateFailed
		_, action, err := launcher.settleRuntime(t.Context(), time.Second, starting, func(context.Context) (RuntimeHealth, error) {
			return failed, nil
		})
		if err != nil || action != runtimeStopRestart {
			t.Fatalf("settleRuntime = %d, %v; want restart", action, err)
		}
	})
	t.Run("draining disappearance spawns", func(t *testing.T) {
		draining := ready
		draining.Draining = true
		_, action, err := launcher.settleRuntime(t.Context(), time.Second, draining, func(context.Context) (RuntimeHealth, error) {
			return RuntimeHealth{}, fmt.Errorf("gone: %w", ErrNoPeer)
		})
		if err != nil || action != runtimeSpawn {
			t.Fatalf("settleRuntime = %d, %v; want spawn", action, err)
		}
	})
	t.Run("draining deadline stops", func(t *testing.T) {
		draining := ready
		draining.Draining = true
		_, action, err := launcher.settleRuntime(t.Context(), time.Millisecond, draining, func(context.Context) (RuntimeHealth, error) {
			t.Fatal("observer called after settlement deadline")
			return RuntimeHealth{}, nil
		})
		if err != nil || action != runtimeStopRestart {
			t.Fatalf("settleRuntime = %d, %v; want restart", action, err)
		}
	})
	t.Run("busy unhealthy settles then stops", func(t *testing.T) {
		busy := ready
		busy.State = dkdaemon.StateDegraded
		busy.Busy = true
		idle := busy
		idle.Busy = false
		_, action, err := launcher.settleRuntime(t.Context(), time.Second, busy, func(context.Context) (RuntimeHealth, error) {
			return idle, nil
		})
		if err != nil || action != runtimeStopRestart {
			t.Fatalf("settleRuntime = %d, %v; want restart", action, err)
		}
	})
	t.Run("busy unhealthy deadline still stops", func(t *testing.T) {
		busy := ready
		busy.State = dkdaemon.StateDegraded
		busy.Busy = true
		_, action, err := launcher.settleRuntime(t.Context(), time.Millisecond, busy, func(context.Context) (RuntimeHealth, error) {
			t.Fatal("observer called after settlement deadline")
			return RuntimeHealth{}, nil
		})
		if err != nil || action != runtimeStopRestart {
			t.Fatalf("settleRuntime = %d, %v; want restart", action, err)
		}
	})
}

func TestLauncherStopRequiresApprovedRoleExecutable(t *testing.T) {
	tests := []struct {
		name       string
		roleTarget string
		wantErr    bool
	}{
		{name: "exact role", wantErr: false},
		{name: "foreign executable", roleTarget: "/bin/true", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, err := os.MkdirTemp("/tmp", "cci-control-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			t.Setenv("HOME", home)
			t.Setenv(stopExecHelperEnv, "1")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			target, err := filepath.EvalSymlinks(executable)
			if err != nil {
				t.Fatal(err)
			}
			roleTarget := tt.roleTarget
			if roleTarget == "" {
				roleTarget = target
			}
			rolePath := filepath.Join(home, "control-role")
			if err := os.Symlink(roleTarget, rolePath); err != nil {
				t.Fatal(err)
			}
			const app = ".cc-interact-control-test"
			role := daemonrole.Classifier{RoleID: "com.yasyf.cc-interact.control-test", RolePath: rolePath}
			t.Setenv("CC_INTERACT_STOP_ARGS_FILE", filepath.Join(home, "stop-args.json"))
			t.Setenv("CC_INTERACT_APP", app)
			t.Setenv("CC_INTERACT_WIRE_BUILD", WireBuild)
			t.Setenv("CC_INTERACT_RUNTIME_BUILD", "1.0.0")
			t.Setenv("CC_INTERACT_ROLE_ID", role.RoleID)
			t.Setenv("CC_INTERACT_ROLE_PATH", role.RolePath)
			command := exec.Command(executable, "-test.run=TestLauncherControlHelper")
			command.Env = append(os.Environ(),
				controlHelperEnv+"=1", "HOME="+home, "CC_INTERACT_APP="+app,
				"CC_INTERACT_WIRE_BUILD="+WireBuild, "CC_INTERACT_RUNTIME_BUILD=1.0.0",
				"CC_INTERACT_ROLE_ID="+role.RoleID, "CC_INTERACT_ROLE_PATH="+role.RolePath,
			)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			})
			p := paths.Paths{App: app}
			waitForSocket(t, p.SocketPath())
			launcher := Launcher{
				Paths: p, WireBuild: WireBuild, RuntimeBuild: "1.0.0",
				Args: []string{"daemon"}, StopArgs: []string{"-test.run=TestLauncherStopExecHelper"}, DaemonRole: role,
			}
			err = launcher.Stop(context.Background(), 5*time.Second)
			if tt.wantErr {
				if err == nil {
					t.Fatal("foreign executable performed protected stop")
				}
				return
			}
			if err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("control helper exit: %v", err)
			}
			stopped = true
		})
	}
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear", socket)
}
