package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
)

// peerAdapter adapts the frozen-protocol control client to daemonkit's daemon.Peer.
type peerAdapter struct {
	c         *Client
	incumbent dkdaemon.Health
	shutdown  bool
}

// Health resolves the incumbent's pid from the socket peer credential — the Reply
// carries none, and daemonkit revalidates the pid before any signal.
func (p *peerAdapter) Health(context.Context) (dkdaemon.Health, error) {
	if p.shutdown {
		pid, err := p.c.peerPID()
		if err != nil {
			return dkdaemon.Health{}, err
		}
		health := p.incumbent
		health.PID = pid
		return health, nil
	}
	reply, err := p.c.Health()
	if err != nil {
		return dkdaemon.Health{}, err
	}
	if !reply.OK {
		return dkdaemon.Health{}, fmt.Errorf("holder health not ok: %s", reply.Error)
	}
	pid, err := p.c.peerPID()
	if err != nil {
		return dkdaemon.Health{}, fmt.Errorf("holder peer pid: %w", err)
	}
	p.incumbent = dkdaemon.Health{Version: reply.DaemonVersion, PID: pid, State: dkdaemon.StateHealthy}
	return p.incumbent, nil
}

func (p *peerAdapter) Shutdown(context.Context) error {
	_, err := p.c.Shutdown()
	if err == nil {
		p.shutdown = true
	}
	return err
}

// Handoff is unreachable: cc-interact advertises no handoff feature, so the
// RequestDaemon shutdown->SIGKILL ladder evicts instead of a handoff.
func (p *peerAdapter) Handoff(context.Context) error {
	return errors.New("cc-interact daemon does not support handoff")
}

// evict is SingleEntrant's contention hook: daemonkit's version-gated takeover,
// refusing under a same-or-newer holder.
func (s *Server) evict(ctx context.Context) (bool, error) {
	peer := &peerAdapter{c: NewClient(s.socket)}
	outcome, err := dkdaemon.Run(ctx, dkdaemon.TakeoverConfig{
		Self:     s.version,
		Peer:     peer,
		Contract: dkdaemon.RequestDaemon,
		WaitMode: dkdaemon.PIDExit,
		Grace:    s.evictTimeout,
	})
	if err != nil {
		return false, err
	}
	switch outcome {
	case dkdaemon.ExitSelf:
		return false, fmt.Errorf("%s daemon already holds the socket (this binary is %s)", s.appName, s.version)
	case dkdaemon.Bind:
		s.waitHolderExit(ctx, peer.incumbent.PID)
		return true, nil
	default:
		return false, fmt.Errorf("unexpected takeover outcome %v", outcome)
	}
}

// waitHolderExit polls for the evicted holder's process exit so the successor's
// HTTP handshake and TCP port are not contended by a dying predecessor. ESRCH
// (gone) ends the wait; a timeout logs and returns, rewriting the handshake at
// most once.
func (s *Server) waitHolderExit(ctx context.Context, pid int) {
	if pid <= 1 || pid == os.Getpid() {
		return
	}
	deadline := time.Now().Add(s.evictTimeout)
	for time.Now().Before(deadline) {
		if err := killProc(pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.log.Printf("holder pid %d still exiting; the handshake may be rewritten once", pid)
}
