package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/version"
	"github.com/yasyf/daemonkit/wire"
)

// UpgradeTimeout bounds an exact-build daemon transition.
const UpgradeTimeout = 30 * time.Second

// Launcher starts, version-gates, and connects to one cc-interact daemon.
type Launcher struct {
	Paths   paths.Paths
	Version string
	Args    []string
}

// NewClient connects to the exact daemon build selected by this launcher.
func (l Launcher) NewClient(ctx context.Context) (*Client, error) {
	return NewClient(ctx, ClientConfig{Socket: l.Paths.SocketPath(), Build: l.Version})
}

// EnsureCurrent starts or upgrades the daemon and waits for an exact build.
func (l Launcher) EnsureCurrent(ctx context.Context, timeout time.Duration) error {
	if err := l.Paths.EnsureStateDir(); err != nil {
		return err
	}
	if err := l.Paths.EnsureLockDir(); err != nil {
		return err
	}
	peer := l.peer()
	defer func() { _ = peer.Close() }()
	if health, err := peer.Health(ctx); err == nil && version.Newer(health.Build, l.Version) {
		return fmt.Errorf("daemon build %s is newer than client build %s", health.Build, l.Version)
	}
	spawn := proc.Spawn{
		Socket:  l.Paths.SocketPath(),
		LogPath: l.Paths.LogPath(),
		Args:    l.Args,
		Timeout: timeout,
		Available: func() bool {
			probeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			health, err := peer.Health(probeCtx)
			return err == nil && health.Build == l.Version && health.Protocol == int(wire.ProtocolVersion)
		},
		CanHost: func() error { return nil },
	}
	return dkdaemon.EnsureCurrent(ctx, dkdaemon.EnsureConfig{
		Peer: peer, Protocol: int(wire.ProtocolVersion), LockPath: l.Paths.StartLockPath(),
		Ensure: spawn.EnsureRunning, Timeout: timeout,
	}, l.Version)
}

// EnsureCurrentIfRunning upgrades a running daemon without cold-starting one.
func (l Launcher) EnsureCurrentIfRunning(ctx context.Context) error {
	peer := l.peer()
	health, err := peer.Health(ctx)
	_ = peer.Close()
	if errors.Is(err, dkdaemon.ErrNoPeer) {
		return err
	}
	if err != nil {
		return fmt.Errorf("probe daemon: %w", err)
	}
	if health.Build == l.Version && health.Protocol == int(wire.ProtocolVersion) {
		return nil
	}
	return l.EnsureCurrent(ctx, UpgradeTimeout)
}

func (l Launcher) peer() *wire.LifecyclePeer {
	return &wire.LifecyclePeer{Config: wire.ClientConfig{
		Dial: wire.UnixDialer(l.Paths.SocketPath()), Build: l.Version,
	}}
}
