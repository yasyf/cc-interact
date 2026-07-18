package daemon

import (
	"context"
	"time"

	"github.com/yasyf/cc-interact/paths"
	"github.com/yasyf/cc-interact/version"
	"github.com/yasyf/daemonkit/proc"
)

// UpgradeTimeout bounds EnsureCurrent's worst case: an eviction's graceful
// shutdown, SIGKILL, and process-exit waits plus the new daemon's boot.
const UpgradeTimeout = 30 * time.Second

// Launcher lazily starts and version-gates the daemon from the client side. It
// re-execs this binary with Args (e.g. ["daemon"]) detached so the daemon
// outlives the CLI.
type Launcher struct {
	Paths   paths.Paths
	Version string   // this binary's version, for same-or-newer-wins
	Args    []string // subcommand to exec the daemon
}

func (l Launcher) client() *Client { return NewClient(l.Paths.SocketPath()) }

// EnsureCurrent returns once a daemon at least as new as this binary answers,
// spawning a detached one otherwise; a strictly older daemon is replaced. The
// availability gate is version-gated, not mere reachability, since the old daemon
// keeps answering throughout its own eviction.
func (l Launcher) EnsureCurrent(timeout time.Duration) error {
	// The spawn side opens the child log before any daemon exists, so a cold
	// start must create the state dir here, not in daemon.New.
	if err := l.Paths.EnsureStateDir(); err != nil {
		return err
	}
	c := l.client()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sp := proc.Spawn{
		Socket:    l.Paths.SocketPath(),
		LogPath:   l.Paths.LogPath(),
		Args:      l.Args,
		Timeout:   timeout,
		Available: func() bool { return l.currentVersion(c) },
		CanHost:   func() error { return nil },
	}
	return sp.EnsureRunning(ctx)
}

// EnsureCurrentIfRunning replaces a reachable strictly-older daemon exactly as
// EnsureCurrent does (and likewise accepts a same-or-newer one), but never
// cold-spawns one when nothing answers: it is for hooks, which must not boot
// daemons. Returns ErrDaemonUnavailable when no daemon is running.
func (l Launcher) EnsureCurrentIfRunning() error {
	if !l.client().Available() {
		return ErrDaemonUnavailable
	}
	return l.EnsureCurrent(UpgradeTimeout)
}

// currentVersion reports whether a reachable daemon is at least this binary's
// version — same-or-newer is current, only strictly older needs replacing.
func (l Launcher) currentVersion(c *Client) bool {
	resp, err := c.Health()
	return err == nil && resp.OK && !version.Newer(l.Version, resp.DaemonVersion)
}
