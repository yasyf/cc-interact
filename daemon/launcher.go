package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/daemonrole"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/version"
	"github.com/yasyf/daemonkit/wire"
)

// UpgradeTimeout bounds an exact-build daemon transition.
const UpgradeTimeout = 30 * time.Second

// Launcher starts, version-gates, and connects to one cc-interact daemon.
type Launcher struct {
	Paths          paths.Paths
	Version        string
	LifecycleBuild string
	Args           []string
	DaemonRole     daemonrole.Classifier
}

// NewClient connects to the exact daemon build selected by this launcher.
func (l Launcher) NewClient(ctx context.Context) (*Client, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	return NewClient(ctx, ClientConfig{
		Socket: l.Paths.SocketPath(), Build: l.Version, LifecycleBuild: l.LifecycleBuild,
	})
}

// EnsureCurrent starts or upgrades the daemon and waits for an exact build.
func (l Launcher) EnsureCurrent(ctx context.Context, timeout time.Duration) (err error) {
	if err := l.validate(); err != nil {
		return err
	}
	if err := l.Paths.EnsureStateDir(); err != nil {
		return err
	}
	if err := l.Paths.EnsureLockDir(); err != nil {
		return err
	}
	peer := l.peer()
	defer func() { err = errors.Join(err, peer.Close()) }()
	if health, err := peer.Health(ctx); err == nil && version.Newer(health.Build, l.LifecycleBuild) {
		return fmt.Errorf("daemon build %s is newer than client build %s", health.Build, l.LifecycleBuild)
	}
	spawn := l.spawn(peer, timeout)
	return dkdaemon.EnsureCurrent(ctx, dkdaemon.EnsureConfig{
		Peer: peer, Protocol: int(wire.ProtocolVersion), LockPath: l.Paths.StartLockPath(),
		Ensure: spawn.EnsureRunning, Timeout: timeout,
	}, l.LifecycleBuild)
}

func (l Launcher) spawn(peer *wire.LifecyclePeer, timeout time.Duration) proc.Spawn {
	return proc.Spawn{
		Socket:   l.Paths.SocketPath(),
		LogPath:  l.Paths.LogPath(),
		Args:     l.Args,
		ExecPath: l.DaemonRole.RolePath,
		Timeout:  timeout,
		Available: func() bool {
			probeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			health, err := peer.Health(probeCtx)
			return err == nil && health.Build == l.LifecycleBuild && health.Protocol == int(wire.ProtocolVersion)
		},
		CanHost: func() error { return nil },
	}
}

// EnsureCurrentIfRunning upgrades a running daemon without cold-starting one.
func (l Launcher) EnsureCurrentIfRunning(ctx context.Context) error {
	if err := l.validate(); err != nil {
		return err
	}
	peer := l.peer()
	health, err := peer.Health(ctx)
	closeErr := peer.Close()
	if errors.Is(err, dkdaemon.ErrNoPeer) {
		return errors.Join(err, closeErr)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("probe daemon: %w", err), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close daemon probe: %w", closeErr)
	}
	if health.Build == l.LifecycleBuild && health.Protocol == int(wire.ProtocolVersion) {
		return nil
	}
	return l.EnsureCurrent(ctx, UpgradeTimeout)
}

func (l Launcher) validate() error {
	if l.Version == "" {
		return errors.New("daemon: business build is required")
	}
	if l.LifecycleBuild == "" {
		return errors.New("daemon: lifecycle build is required")
	}
	if err := l.DaemonRole.Validate(); err != nil {
		return fmt.Errorf("daemon: validate launcher role: %w", err)
	}
	return nil
}

func (l Launcher) peer() *wire.LifecyclePeer {
	return &wire.LifecyclePeer{Config: wire.ClientConfig{
		Dial: wire.UnixDialer(l.Paths.SocketPath()), Build: l.Version, LifecycleBuild: l.LifecycleBuild,
	}}
}
