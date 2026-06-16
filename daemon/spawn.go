package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/yasyf/cc-interact/paths"
	"github.com/yasyf/cc-interact/version"
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

// EnsureCurrent returns once a daemon at least as new as this binary is
// reachable, spawning a detached one if none is. A strictly older daemon is
// replaced on first contact: the spawned daemon's listen() evicts it. A
// same-or-newer daemon is accepted as-is. A flock around the spawn serializes
// simultaneous cold starts so only one process binds the socket.
func (l Launcher) EnsureCurrent(timeout time.Duration) error {
	c := l.client()
	if l.currentVersion(c) {
		return nil
	}
	deadline := time.Now().Add(timeout)
	return l.underStartLock(deadline, func() error {
		if l.currentVersion(c) { // a concurrent command already upgraded
			return nil
		}
		if err := l.spawnDaemon(); err != nil {
			return err
		}
		// Poll the version, not mere availability: the old daemon keeps answering
		// on the socket throughout its own eviction.
		return waitFor(deadline, func() bool { return l.currentVersion(c) },
			"daemon did not reach the current version in time")
	})
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

// underStartLock runs fn while holding the exclusive start flock, waiting for it
// until deadline.
func (l Launcher) underStartLock(deadline time.Time, fn func() error) error {
	if err := l.Paths.EnsureLockDir(); err != nil {
		return err
	}
	lock, err := os.OpenFile(l.Paths.StartLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open start lock: %w", err)
	}
	defer lock.Close()

	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("acquire start lock: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("timed out acquiring daemon start lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	return fn()
}

// spawnDaemon starts a detached `<exe> <Args...>` (Setsid + Release) that
// outlives the CLI, with stdout/stderr appended to the daemon log so boot,
// eviction, and panic output survive the process.
func (l Launcher) spawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	// Append, never truncate: during an eviction the dying daemon's last words
	// must land in the log alongside the successor's boot line.
	logFile, err := os.OpenFile(l.Paths.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(exe, l.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

func waitFor(deadline time.Time, ok func() bool, timeoutMsg string) error {
	for time.Now().Before(deadline) {
		if ok() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New(timeoutMsg)
}
