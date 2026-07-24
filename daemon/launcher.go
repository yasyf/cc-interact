package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/service"
	"github.com/yasyf/daemonkit/trust"
	"github.com/yasyf/daemonkit/version"
	"github.com/yasyf/daemonkit/wire"
)

// UpgradeTimeout bounds an exact-build daemon transition.
const UpgradeTimeout = 30 * time.Second

// ErrNoPeer means the local runtime endpoint is provably absent.
var ErrNoPeer = errors.New("daemon: no peer")

type runtimeAction uint8

const (
	runtimeObserve runtimeAction = iota
	runtimeNoop
	runtimeSpawn
	runtimeStopUpgrade
	runtimeStopRestart
)

// Launcher starts, version-gates, and connects to one cc-interact daemon.
type Launcher struct {
	Paths        paths.Paths
	WireBuild    string
	RuntimeBuild string
	Agent        service.Agent
	Roles        Roles
}

// NewClient connects to the exact business protocol selected by this launcher.
func (l Launcher) NewClient(ctx context.Context) (*Client, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	return NewClient(ctx, ClientConfig{Socket: l.Paths.SocketPath(), WireBuild: l.WireBuild, Role: l.Roles.Business})
}

// EnsureCurrent starts or upgrades the daemon and waits for exact product readiness.
func (l Launcher) EnsureCurrent(ctx context.Context, timeout time.Duration) (err error) {
	if err := l.prepare(); err != nil {
		return err
	}
	operationCtx, cancel := boundedContext(ctx, timeout)
	defer cancel()
	return l.withStartLock(operationCtx, timeout, func() error {
		return l.withController(operationCtx, func(controller *service.Controller) error {
			health, probeErr := l.runtimeHealth(operationCtx)
			if errors.Is(probeErr, ErrNoPeer) {
				return l.start(operationCtx, controller, timeout)
			}
			if probeErr != nil {
				return probeErr
			}
			health, action, err := l.settleRuntime(operationCtx, timeout, health, l.runtimeHealth)
			if err != nil {
				return err
			}
			switch action {
			case runtimeNoop:
				return nil
			case runtimeSpawn:
				return l.start(operationCtx, controller, timeout)
			case runtimeStopUpgrade:
				if err := l.stopRuntime(operationCtx, controller, health); err != nil {
					return err
				}
			case runtimeStopRestart:
				if err := l.stopRuntime(operationCtx, controller, health); err != nil {
					return err
				}
			default:
				return fmt.Errorf("daemon: invalid runtime action %d for generation %s", action, health.ProcessGeneration)
			}
			if err := controller.Converge(operationCtx, nil); err != nil {
				return fmt.Errorf("daemon: settle prior service: %w", err)
			}
			if err := l.start(operationCtx, controller, timeout); err != nil {
				if health, probeErr := l.runtimeHealth(operationCtx); probeErr == nil && version.Newer(health.RuntimeBuild, l.RuntimeBuild) {
					return fmt.Errorf("daemon runtime build %s is newer than client build %s", health.RuntimeBuild, l.RuntimeBuild)
				}
				return err
			}
			return nil
		})
	})
}

func (l Launcher) start(ctx context.Context, controller *service.Controller, timeout time.Duration) error {
	if err := controller.Converge(ctx, []service.Agent{l.Agent}); err != nil {
		return fmt.Errorf("daemon: converge service: %w", err)
	}
	_, err := wire.AcquireReadyRuntime(ctx, l.runtimeClientConfig(l.Roles.Lifecycle, timeout), l.RuntimeBuild)
	return err
}

// EnsureCurrentIfRunning upgrades a running daemon without cold-starting one.
func (l Launcher) EnsureCurrentIfRunning(ctx context.Context) error {
	if err := l.validate(); err != nil {
		return err
	}
	health, err := l.runtimeHealth(ctx)
	if errors.Is(err, ErrNoPeer) {
		return ErrNoPeer
	}
	if err == nil {
		if version.Newer(health.RuntimeBuild, l.RuntimeBuild) {
			return fmt.Errorf("daemon runtime build %s is newer than client build %s", health.RuntimeBuild, l.RuntimeBuild)
		}
		if l.ready(health) {
			return nil
		}
	}
	return l.EnsureCurrent(ctx, UpgradeTimeout)
}

// Stop returns ErrNoPeer for proven absence or performs daemonkit's durable,
// receipt-authenticated stop before removing the service.
func (l Launcher) Stop(ctx context.Context, timeout time.Duration) error {
	if err := l.prepare(); err != nil {
		return err
	}
	controlCtx, cancel := boundedContext(ctx, timeout)
	defer cancel()
	return l.withStartLock(controlCtx, timeout, func() error {
		health, err := l.runtimeHealth(controlCtx)
		if errors.Is(err, ErrNoPeer) {
			return err
		}
		if err != nil {
			return err
		}
		return l.withController(controlCtx, func(controller *service.Controller) error {
			if err := l.stopRuntime(controlCtx, controller, health); err != nil {
				return err
			}
			return controller.Converge(controlCtx, nil)
		})
	})
}

func (l Launcher) stopRuntime(
	ctx context.Context,
	controller *service.Controller,
	health RuntimeHealth,
) error {
	_, err := controller.StopRuntime(ctx, service.StopRuntimeRequest{
		OperationID:          fmt.Sprintf("%s.stop.v1:%s", l.Agent.Label, health.ProcessGeneration),
		RuntimeClientConfig:  l.runtimeClientConfig(l.Roles.StopControl, UpgradeTimeout),
		ExpectedRuntimeBuild: health.RuntimeBuild,
		ControlRole:          l.Roles.StopControl,
	})
	return err
}

func (l Launcher) runtimeHealth(ctx context.Context) (health RuntimeHealth, err error) {
	client, err := NewClient(ctx, ClientConfig{Socket: l.Paths.SocketPath(), WireBuild: l.WireBuild, Role: l.Roles.Business})
	if err != nil {
		if provesNoRuntime(err) {
			return RuntimeHealth{}, fmt.Errorf("daemon runtime health: %w", ErrNoPeer)
		}
		return RuntimeHealth{}, err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	return client.RuntimeHealth(ctx)
}

func (l Launcher) settleRuntime(
	ctx context.Context,
	timeout time.Duration,
	health RuntimeHealth,
	observe func(context.Context) (RuntimeHealth, error),
) (RuntimeHealth, runtimeAction, error) {
	observeCtx, cancel := boundedContext(ctx, timeout)
	defer cancel()
	for {
		action, err := l.runtimeAction(health)
		if err != nil || action != runtimeObserve {
			return health, action, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-observeCtx.Done():
			timer.Stop()
			return health, runtimeStopRestart, nil
		case <-timer.C:
		}
		next, err := observe(observeCtx)
		if errors.Is(err, ErrNoPeer) {
			return RuntimeHealth{}, runtimeSpawn, nil
		}
		if err != nil {
			return health, runtimeObserve, err
		}
		health = next
	}
}

func (l Launcher) runtimeAction(health RuntimeHealth) (runtimeAction, error) {
	if version.Newer(health.RuntimeBuild, l.RuntimeBuild) {
		return runtimeObserve, fmt.Errorf("daemon runtime build %s is newer than client build %s", health.RuntimeBuild, l.RuntimeBuild)
	}
	if health.RuntimeBuild == "" || health.RuntimeProtocol != int(wire.ProtocolVersion) || health.ProcessGeneration == "" {
		return runtimeObserve, fmt.Errorf(
			"daemon runtime identity is incomplete: build=%q protocol=%d generation=%q",
			health.RuntimeBuild, health.RuntimeProtocol, health.ProcessGeneration,
		)
	}
	if health.RuntimeBuild != l.RuntimeBuild {
		return runtimeStopUpgrade, nil
	}
	if health.Draining {
		return runtimeObserve, nil
	}
	if !health.Ready {
		if health.State == dkdaemon.StateFailed {
			return runtimeStopRestart, nil
		}
		return runtimeObserve, nil
	}
	if health.State == dkdaemon.StateHealthy {
		return runtimeNoop, nil
	}
	if health.State != dkdaemon.StateFailed && health.Busy {
		return runtimeObserve, nil
	}
	return runtimeStopRestart, nil
}

func provesNoRuntime(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func processStore(p paths.Paths) *proc.FileStore {
	return &proc.FileStore{Path: filepath.Join(p.StateDir(), "processes.db")}
}

func stopProcessStore(p paths.Paths) *proc.FileStore {
	return &proc.FileStore{Path: filepath.Join(p.StateDir(), "stop-processes.db")}
}

func (l Launcher) withController(ctx context.Context, run func(*service.Controller) error) (err error) {
	controller, err := service.NewController(ctx, service.ControllerConfig{
		StatePath:   filepath.Join(l.Paths.StateDir(), "services.db"),
		ProcessPath: stopProcessStore(l.Paths).Path,
		WorkerLimit: 1,
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, controller.Close(context.WithoutCancel(ctx))) }()
	return run(controller)
}

func (l Launcher) runtimeClientConfig(role trust.PeerRole, timeout time.Duration) wire.RuntimeClientConfig {
	return wire.RuntimeClientConfig{
		Client: wire.ClientConfig{
			Dial: wire.UnixDialer(l.Paths.SocketPath()), WireBuild: l.WireBuild,
			Role: role, MaxFrame: maxFrameBytes,
		},
		NoProgressTimeout: timeout,
	}
}

func (l Launcher) ready(health RuntimeHealth) bool {
	return health.RuntimeBuild == l.RuntimeBuild &&
		health.RuntimeProtocol == int(wire.ProtocolVersion) &&
		health.ProcessGeneration != "" &&
		health.Ready &&
		health.State == dkdaemon.StateHealthy && !health.Draining
}

func (l Launcher) prepare() error {
	if err := l.validate(); err != nil {
		return err
	}
	if err := l.Paths.EnsureStateDir(); err != nil {
		return err
	}
	return l.Paths.EnsureLockDir()
}

func (l Launcher) validate() error {
	if l.WireBuild != WireBuild {
		return fmt.Errorf("daemon: wire build %q, want exactly %q", l.WireBuild, WireBuild)
	}
	if l.RuntimeBuild == "" {
		return errors.New("daemon: runtime build is required")
	}
	if _, err := l.Agent.Plist(); err != nil {
		return fmt.Errorf("daemon: validate service agent: %w", err)
	}
	if l.Roles.Business == "" || l.Roles.Lifecycle == "" || l.Roles.StopControl == "" {
		return errors.New("daemon: launcher roles are required")
	}
	return nil
}

func (l Launcher) withStartLock(ctx context.Context, timeout time.Duration, run func() error) (err error) {
	deadline := timeout
	if deadline <= 0 {
		deadline = 5 * time.Second
	}
	lock, err := (proc.FileLockSpec{
		Path: l.Paths.StartLockPath(), Mode: proc.FileLockExclusive, Deadline: deadline,
	}).Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire start lock %s: %w", l.Paths.StartLockPath(), err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	return run()
}

func boundedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = UpgradeTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
