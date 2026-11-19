package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yasyf/cc-interact/version"
	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/durable"
	"github.com/yasyf/daemonkit/paths"
)

// BootAllowance is how long a replacement daemon may take, once the incumbent
// has left, to boot and report Ready.
const BootAllowance = 30 * time.Second

// ErrNoPeer is daemonkit's proven-absence refusal: no daemon is listening.
var ErrNoPeer = daemonkit.ErrAbsent

// ErrUpgradeDeferred refuses to begin replacing a serving daemon when the
// caller's deadline cannot cover the whole transition. The incumbent keeps
// serving, rather than draining under a caller killed before its replacement
// is Ready.
var ErrUpgradeDeferred = errors.New("daemon: no time to finish an upgrade")

// ErrIncumbentNewer refuses an upgrade that would put an older release back in
// a newer one's place. daemonkit converges on Health.Build, which proves two
// builds differ but never which came first, so a downgrade otherwise reads as
// an ordinary transition; the ordering rides HealthDetail.RuntimeBuild instead.
var ErrIncumbentNewer = errors.New("daemon: incumbent runs a newer release")

// Launcher starts, converges, and connects to one cc-interact daemon. Every
// field is the value the serving half reads from Config: Daemon is the shared
// identity built through Spec, Paths is the state directory that identity
// serves out of, and RuntimeBuild is the release this launcher orders itself
// by.
type Launcher struct {
	Daemon       daemonkit.Daemon
	Paths        paths.Paths
	RuntimeBuild string
}

// NewClient opens the business lane of the launcher's daemon.
func (l Launcher) NewClient() (*Client, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	return NewClient(l.Daemon)
}

// EnsureCurrent makes the daemon be the exact build of this launcher's own
// Program, ready and serving, cold-starting one when none runs. A newer
// incumbent refuses with ErrIncumbentNewer instead of being rolled back.
// Ensures of one daemon run one at a time, so a launcher that probes while
// another replaces the daemon reads the build that replacement left serving
// rather than the gap before it. A transition runs on TransitionBudget,
// whatever the caller's own deadline, and holds the turn until the
// replacement is Ready or has failed; a caller whose deadline cannot cover it
// gets ErrUpgradeDeferred before anything is drained.
func (l Launcher) EnsureCurrent(ctx context.Context) error {
	return l.ensure(ctx, true)
}

// EnsureCurrentIfRunning upgrades a running daemon without cold-starting one,
// returning ErrNoPeer for proven absence. An incumbent that is already leaving
// is running, not absent: Ensure's own drain-observation ladder settles it and
// starts the wanted build in its place. A newer incumbent refuses with
// ErrIncumbentNewer. It waits its turn, budgets, and defers as EnsureCurrent
// does.
func (l Launcher) EnsureCurrentIfRunning(ctx context.Context) error {
	return l.ensure(ctx, false)
}

// TransitionBudget is how long replacing a serving daemon may take: its drain,
// which daemonkit bounds by the daemon's Shutdown grace (launchd's ExitTimeOut)
// and observes on half of what an eviction has left, then the replacement's
// BootAllowance.
func (l Launcher) TransitionBudget() time.Duration {
	drain := time.Duration(l.Daemon.ShutdownGrace())
	return drain + max(drain, BootAllowance)
}

func (l Launcher) ensure(ctx context.Context, coldStart bool) error {
	client, err := l.open()
	if err != nil {
		return err
	}
	defer closeProbe(client)
	budget := l.TransitionBudget()
	turnCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	release, err := l.awaitEnsureTurn(turnCtx)
	if err != nil {
		return err
	}
	defer release()
	current, err := l.probeIncumbent(turnCtx, client)
	switch {
	case err == nil, errors.Is(err, daemonkit.ErrDraining), errors.Is(err, daemonkit.ErrProtocolMismatch):
		if !current && !affords(ctx, budget) {
			return fmt.Errorf("%w: the caller's deadline leaves less than the %s a transition takes", ErrUpgradeDeferred, budget)
		}
	case errors.Is(err, daemonkit.ErrAbsent):
		if !coldStart {
			return ErrNoPeer
		}
	default:
		return err
	}
	if err := l.claimPlacement(current); err != nil {
		return err
	}
	transitionCtx, cancelTransition := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancelTransition()
	if _, err := client.Ensure(transitionCtx); err != nil {
		return fmt.Errorf("daemon: ensure: %w", err)
	}
	return nil
}

func (l Launcher) claimPlacement(current bool) error {
	newest, err := l.newestPlaced()
	if err != nil {
		return err
	}
	if !current && newest != "" && version.Newer(newest, l.RuntimeBuild) {
		return fmt.Errorf("%w: %s was already placed, so this build %s never replaces it", ErrIncumbentNewer, newest, l.RuntimeBuild)
	}
	if !version.IsRelease(l.RuntimeBuild) || (newest != "" && !version.Newer(l.RuntimeBuild, newest)) {
		return nil
	}
	if err := durable.WriteFile(newestPlacedPath(l.Paths), []byte(l.RuntimeBuild), 0o600); err != nil {
		return fmt.Errorf("daemon: record the newest placed build: %w", err)
	}
	return nil
}

func (l Launcher) newestPlaced() (string, error) {
	data, err := os.ReadFile(newestPlacedPath(l.Paths))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("daemon: read the newest placed build: %w", err)
	}
	return string(data), nil
}

func affords(ctx context.Context, budget time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= budget
}

func (l Launcher) awaitEnsureTurn(ctx context.Context) (func(), error) {
	if err := l.Paths.EnsureLockDir(); err != nil {
		return nil, fmt.Errorf("daemon: create lock dir: %w", err)
	}
	lock, err := os.OpenFile(ensureLockPath(l.Paths), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open ensure lock: %w", err)
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = lock.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = lock.Close()
			return nil, fmt.Errorf("daemon: ensure lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, fmt.Errorf("daemon: wait for another ensure: %w", ctx.Err())
		case <-time.After(ensureTurnPoll):
		}
	}
}

const (
	ensureTurnPoll = 50 * time.Millisecond
	closeTimeout   = 5 * time.Second
)

func ensureLockPath(p paths.Paths) string { return filepath.Join(p.LockDir(), "ensure.lock") }

func newestPlacedPath(p paths.Paths) string {
	return filepath.Join(p.StateDir(), "newest-placed-build")
}

// probeIncumbent passes daemonkit's ErrAbsent, ErrDraining, and
// ErrProtocolMismatch back unwrapped, so each ensure path classifies an
// absence, a departure, or an older wire protocol on its own terms. It reports
// whether the incumbent already serves this launcher's release, Ready.
func (l Launcher) probeIncumbent(ctx context.Context, client *daemonkit.Client) (bool, error) {
	health, err := client.Health(ctx)
	switch {
	case err == nil:
	case errors.Is(err, daemonkit.ErrAbsent), errors.Is(err, daemonkit.ErrDraining), errors.Is(err, daemonkit.ErrProtocolMismatch):
		return false, err
	default:
		return false, fmt.Errorf("daemon: probe incumbent: %w", err)
	}
	if err := l.refuseRollback(health); err != nil {
		return false, err
	}
	incumbent, _ := reportedBuild(health.Detail)
	return health.Phase == daemonkit.PhaseReady && incumbent == l.RuntimeBuild, nil
}

// refuseRollback refuses only a rollback this launcher can prove. Detail that
// is absent, unreadable, or carries no release names a daemon whose ordering
// this build cannot establish, and ordering it by assumption would strand the
// very upgrade that introduces the channel.
func (l Launcher) refuseRollback(health daemonkit.Health) error {
	incumbent, ok := reportedBuild(health.Detail)
	if !ok {
		return nil
	}
	if version.Newer(incumbent, l.RuntimeBuild) {
		return fmt.Errorf("%w: %s supersedes this build %s", ErrIncumbentNewer, incumbent, l.RuntimeBuild)
	}
	return nil
}

func reportedBuild(detail []byte) (string, bool) {
	if len(detail) == 0 {
		return "", false
	}
	var reported HealthDetail
	if err := decodeStrict(detail, &reported); err != nil {
		return "", false
	}
	return reported.RuntimeBuild, reported.RuntimeBuild != ""
}

// Stop leaves nothing serving at this daemon's label and no LaunchAgent behind
// it. daemonkit runs the whole sequence under the start lock Ensure holds, so
// a concurrent ensure can neither re-apply the agent this call removed nor lose
// its own replacement to it, and the agent comes down only once departure is
// proven. Stopping an already stopped daemon succeeds.
//
// It stops through a Daemon that states no Program, daemonkit's own contract:
// Stop renders no LaunchAgent and places nothing, so a stated Program only
// hands its absence proof an executable Ensure alone ever places — and on a
// machine that never ensured this era, resolving that unplaced path fails
// before the agent is ever removed.
func (l Launcher) Stop(ctx context.Context) error {
	if err := l.validate(); err != nil {
		return err
	}
	stopping := l.Daemon
	stopping.Program = daemonkit.Program{}
	client, err := daemonkit.Open(stopping)
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, l.TransitionBudget())
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		return fmt.Errorf("daemon: stop: %w", err)
	}
	return nil
}

func (l Launcher) open() (*daemonkit.Client, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	return daemonkit.Open(l.Daemon)
}

func (l Launcher) validate() error {
	if err := validateSchemas(l.Daemon); err != nil {
		return err
	}
	if l.Paths.App == "" {
		return errors.New("daemon: launcher paths are required")
	}
	if l.RuntimeBuild == "" {
		return errors.New("daemon: runtime build is required")
	}
	return l.Daemon.ValidateForClient()
}

func closeProbe(client *daemonkit.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = client.Close(ctx)
}
