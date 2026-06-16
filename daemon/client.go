package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"
)

// ErrDaemonUnavailable is returned when the control socket cannot be reached.
var ErrDaemonUnavailable = errors.New("daemon unavailable")

const dialTimeout = 500 * time.Millisecond

// Client dials the daemon's control socket. It is the generic envelope RPC: the
// caller fills the Envelope (including the window pid); the client never stamps
// domain identity.
type Client struct {
	socket string
}

// NewClient returns a client for the given control-socket path.
func NewClient(socket string) *Client { return &Client{socket: socket} }

// Available reports whether the daemon answers on the socket.
func (c *Client) Available() bool {
	conn, err := net.DialTimeout("unix", c.socket, dialTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitGone polls until the socket stops accepting connections or timeout elapses.
func (c *Client) WaitGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", c.socket, 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// Do sends one envelope and reads one reply over a fresh connection. The
// envelope's Proto is stamped here. The connection deadline comes from ctx, or
// defaults to the daemon's per-RPC bound when ctx carries none.
func (c *Client) Do(ctx context.Context, env Envelope) (Reply, error) {
	conn, err := net.DialTimeout("unix", c.socket, dialTimeout)
	if err != nil {
		return Reply{}, ErrDaemonUnavailable
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(handleTimeout)
	}
	_ = conn.SetDeadline(deadline)

	env.Proto = ProtocolVersion
	if err := json.NewEncoder(conn).Encode(env); err != nil {
		return Reply{}, err
	}
	var reply Reply
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&reply); err != nil {
		return Reply{}, err
	}
	return reply, nil
}

// Health probes liveness and returns the daemon version.
func (c *Client) Health() (Reply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.Do(ctx, Envelope{Op: OpHealth})
}

// Shutdown asks the daemon to step down.
func (c *Client) Shutdown() (Reply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.Do(ctx, Envelope{Op: OpShutdown})
}
