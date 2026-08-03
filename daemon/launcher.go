package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/yasyf/cc-interact/version"
	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/paths"
)

// UpgradeTimeout bounds an exact-build daemon transition.
const UpgradeTimeout = 30 * time.Second

// ErrNoPeer is daemonkit's proven-absence refusal: no daemon is listening.
var ErrNoPeer = daemonkit.ErrAbsent

// ErrIncumbentNewer refuses an upgrade that would put an older release back in
// a newer one's place. daemonkit converges on Health.Build, an executable
// digest that proves two builds differ but never which came first, so a
// downgrade otherwise reads as an ordinary transition; the ordering rides
// HealthDetail.RuntimeBuild instead.
var ErrIncumbentNewer = errors.New("daemon: incumbent runs a newer release")

// ErrLegacyDaemon reports a pre-0.21 daemon still listening on the state
// directory rather than the label-derived endpoint. Nothing in this build
// speaks its wire era, so the socket file is the whole of the evidence: it can
// be neither probed nor upgraded in place, only stopped.
var ErrLegacyDaemon = errors.New("daemon: a pre-0.21 daemon is running")

// Launcher starts, converges, and connects to one cc-interact daemon. Every
// field is the value the serving half reads from Config: Daemon is the shared
// identity built through Spec, Paths locates what a pre-0.21 daemon left in
// the state directory, and RuntimeBuild is the release this launcher orders
// itself by.
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
func (l Launcher) EnsureCurrent(ctx context.Context, timeout time.Duration) error {
	client, err := l.open()
	if err != nil {
		return err
	}
	operationCtx, cancel := boundedContext(ctx, timeout)
	defer cancel()
	switch err := l.probeIncumbent(operationCtx, client); {
	case err == nil, errors.Is(err, daemonkit.ErrAbsent), errors.Is(err, daemonkit.ErrDraining):
	default:
		return err
	}
	if _, err := client.Ensure(operationCtx); err != nil {
		return fmt.Errorf("daemon: ensure: %w", err)
	}
	return nil
}

// EnsureCurrentIfRunning upgrades a running daemon without cold-starting one,
// returning ErrNoPeer for proven absence and ErrLegacyDaemon when the absence
// is only this era's. An incumbent that is already leaving is running, not
// absent: Ensure's own drain-observation ladder settles it and starts the
// wanted build in its place. A newer incumbent refuses with ErrIncumbentNewer.
func (l Launcher) EnsureCurrentIfRunning(ctx context.Context) error {
	client, err := l.open()
	if err != nil {
		return err
	}
	operationCtx, cancel := boundedContext(ctx, UpgradeTimeout)
	defer cancel()
	switch err := l.probeIncumbent(operationCtx, client); {
	case err == nil, errors.Is(err, daemonkit.ErrDraining):
	case errors.Is(err, daemonkit.ErrAbsent):
		return l.classifyAbsence()
	default:
		return err
	}
	if _, err := client.Ensure(operationCtx); err != nil {
		return fmt.Errorf("daemon: ensure: %w", err)
	}
	return nil
}

// probeIncumbent passes daemonkit's ErrAbsent and ErrDraining back unwrapped,
// so each ensure path classifies an absence on its own terms.
func (l Launcher) probeIncumbent(ctx context.Context, client *daemonkit.Client) error {
	control, err := client.Control(ctx)
	switch {
	case err == nil:
	case errors.Is(err, daemonkit.ErrAbsent), errors.Is(err, daemonkit.ErrDraining):
		return err
	default:
		return fmt.Errorf("daemon: probe incumbent: %w", err)
	}
	defer func() { _ = control.Close(ctx) }()
	health, err := control.Health(ctx)
	if err != nil {
		return fmt.Errorf("daemon: read incumbent health: %w", err)
	}
	return l.refuseRollback(health)
}

// refuseRollback refuses only a rollback this launcher can prove. Detail that
// is absent, unreadable, or carries no release names a daemon whose ordering
// this build cannot establish — a pre-0.21 incumbent among them — and ordering
// it by assumption would strand the very upgrade that introduces the channel.
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

// classifyAbsence names what an absence on the label-derived endpoint means. A
// pre-0.21 daemon listens on the state directory instead and answers no
// handshake this build can speak, so its socket file is the only evidence of it
// left to read.
func (l Launcher) classifyAbsence() error {
	legacy := l.Paths.SocketPath()
	if _, err := os.Stat(legacy); err != nil {
		return ErrNoPeer
	}
	return fmt.Errorf("%w on %s", ErrLegacyDaemon, legacy)
}

// Stop leaves nothing serving at this daemon's label and no LaunchAgent behind
// it, a pre-0.21 markerless one included. daemonkit runs the whole sequence
// under the start lock Ensure holds, so a concurrent ensure can neither
// re-apply the agent this call removed nor lose its own replacement to it, and
// the agent comes down only once departure is proven. Stopping an already
// stopped daemon succeeds.
func (l Launcher) Stop(ctx context.Context, timeout time.Duration) error {
	client, err := l.open()
	if err != nil {
		return err
	}
	stopCtx, cancel := boundedContext(ctx, timeout)
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

func boundedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = UpgradeTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
