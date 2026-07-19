package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/wire"
	"github.com/yasyf/daemonkit/wire/lifeproto"
)

// ErrClientClosed means the persistent client has been permanently closed.
var ErrClientClosed = errors.New("daemon: client closed")

// CallError preserves the transport's proof about one failed operation.
type CallError struct {
	Op      wire.Op
	Outcome wire.Outcome
	Reason  string
	Err     error
}

// Error describes the failed operation and its delivery outcome.
func (e *CallError) Error() string {
	detail := e.Reason
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
	if detail == "" {
		detail = "no terminal response"
	}
	return fmt.Sprintf("daemon: call %s %s: %s", e.Op, e.Outcome, detail)
}

// Unwrap returns the underlying transport error, when present.
func (e *CallError) Unwrap() error { return e.Err }

// ClientConfig identifies one exact daemon build and persistent control socket.
type ClientConfig struct {
	Socket        string
	Build         string
	MaxFrameBytes int
}

type clientSession struct {
	wire       *wire.Client
	generation uint64
	active     int
	stale      bool
}

// Client maintains one generation-aware persistent daemon session. A failed
// operation is never replayed; the next operation establishes a fresh session.
type Client struct {
	cfg ClientConfig

	mu         sync.Mutex
	current    *clientSession
	sessions   map[*clientSession]struct{}
	dialing    chan struct{}
	generation uint64
	closed     bool
}

// NewClient connects and completes the exact v4 build handshake.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	c := &Client{cfg: cfg, sessions: make(map[*clientSession]struct{})}
	session, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	c.generation = 1
	session.generation = c.generation
	c.current = session
	c.sessions[session] = struct{}{}
	return c, nil
}

func (c *Client) dial(ctx context.Context) (*clientSession, error) {
	maxFrame := c.cfg.MaxFrameBytes
	if maxFrame == 0 {
		maxFrame = maxFrameBytes
	}
	client, err := wire.NewClient(ctx, wire.ClientConfig{
		Dial:     wire.UnixDialer(c.cfg.Socket),
		Build:    c.cfg.Build,
		MaxFrame: maxFrame,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: connect: %w", err)
	}
	return &clientSession{wire: client}, nil
}

// Close permanently closes every current or retiring session.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sessions := make([]*clientSession, 0, len(c.sessions))
	for session := range c.sessions {
		sessions = append(sessions, session)
	}
	c.current = nil
	clear(c.sessions)
	c.mu.Unlock()
	var errs []error
	for _, session := range sessions {
		if err := session.wire.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Do dispatches one business operation without replaying it across sessions.
func (c *Client) Do(ctx context.Context, env Envelope) (Reply, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, handleTimeout+time.Second)
		defer cancel()
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return Reply{}, fmt.Errorf("daemon: encode %s: %w", env.Op, err)
	}
	result, err := c.call(ctx, wire.Op(env.Op), payload)
	if err != nil {
		return Reply{}, err
	}
	if result.Response.Err != "" {
		return Reply{}, errors.New(result.Response.Err)
	}
	var reply Reply
	if err := decodeStrict(result.Response.Payload, &reply); err != nil {
		return Reply{}, fmt.Errorf("daemon: decode %s response: %w", env.Op, err)
	}
	return reply, nil
}

// Health returns the connected daemon's exact lifecycle state.
func (c *Client) Health(ctx context.Context) (dkdaemon.Health, error) {
	var response lifeproto.HealthResponse
	if err := c.lifecycle(ctx, wire.Op(lifeproto.OpHealth), lifeproto.NewHealthRequest(), &response); err != nil {
		return dkdaemon.Health{}, err
	}
	return dkdaemon.Health{
		Build: response.Build, Protocol: response.Protocol, PID: response.PID,
		State: dkdaemon.State(response.State), Draining: response.Draining, Busy: response.Busy,
	}, nil
}

// Shutdown requests orderly daemon shutdown.
func (c *Client) Shutdown(ctx context.Context) error {
	var response lifeproto.ShutdownResponse
	if err := c.lifecycle(ctx, wire.Op(lifeproto.OpShutdown), lifeproto.NewShutdownRequest(), &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New("daemon: shutdown not acknowledged")
	}
	return nil
}

func (c *Client) lifecycle(ctx context.Context, op wire.Op, message, response any) error {
	payload, err := lifeproto.Encode(message)
	if err != nil {
		return err
	}
	result, err := c.call(ctx, op, payload)
	if err != nil {
		return err
	}
	if result.Response.Err != "" {
		return errors.New(result.Response.Err)
	}
	if err := decodeStrict(result.Response.Payload, response); err != nil {
		return fmt.Errorf("daemon: decode lifecycle %s: %w", op, err)
	}
	return nil
}

func (c *Client) call(ctx context.Context, op wire.Op, payload []byte) (wire.Result, error) {
	session, err := c.acquire(ctx)
	if err != nil {
		return wire.Result{Outcome: wire.PreSendFailure}, &CallError{Op: op, Outcome: wire.PreSendFailure, Err: err}
	}
	result, callErr := session.wire.Call(ctx, op, "", payload)
	if callErr != nil || result.Outcome != wire.Delivered {
		c.retire(session)
	}
	c.release(session)
	if callErr != nil || result.Outcome != wire.Delivered {
		return result, &CallError{
			Op: op, Outcome: result.Outcome, Reason: result.Response.Reason, Err: callErr,
		}
	}
	return result, nil
}

func (c *Client) acquire(ctx context.Context) (*clientSession, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClientClosed
		}
		if c.current != nil && !c.current.stale {
			session := c.current
			session.active++
			c.mu.Unlock()
			return session, nil
		}
		if c.dialing != nil {
			dialing := c.dialing
			c.mu.Unlock()
			select {
			case <-dialing:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		dialing := make(chan struct{})
		c.dialing = dialing
		c.mu.Unlock()

		session, err := c.dial(ctx)
		c.mu.Lock()
		c.dialing = nil
		if err == nil && !c.closed {
			c.generation++
			session.generation = c.generation
			session.active = 1
			c.current = session
			c.sessions[session] = struct{}{}
		}
		closed := c.closed
		close(dialing)
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if closed {
			_ = session.wire.Close()
			return nil, ErrClientClosed
		}
		return session, nil
	}
}

func (c *Client) retire(session *clientSession) {
	c.mu.Lock()
	session.stale = true
	if c.current == session && c.current.generation == session.generation {
		c.current = nil
	}
	c.mu.Unlock()
}

func (c *Client) release(session *clientSession) {
	var closeSession bool
	c.mu.Lock()
	session.active--
	if session.active < 0 {
		c.mu.Unlock()
		panic("daemon: negative client session references")
	}
	if session.stale && session.active == 0 {
		delete(c.sessions, session)
		closeSession = true
	}
	c.mu.Unlock()
	if closeSession {
		_ = session.wire.Close()
	}
}

func decodeStrict(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
