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

	"github.com/yasyf/daemonkit/trust"
	"github.com/yasyf/daemonkit/wire"
)

// ErrClientClosed means the persistent client has been permanently closed.
var ErrClientClosed = errors.New("daemon: client closed")

var errSessionRetired = errors.New("daemon: session retired after a non-delivered operation")

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

// ClientConfig identifies one exact wire schema and persistent control socket.
type ClientConfig struct {
	Socket        string
	WireBuild     string
	Role          trust.PeerRole
	MaxFrameBytes int
}

type clientSession struct {
	wire       *wire.Client
	generation uint64
	active     int
	stale      bool
}

type closingClientSession struct {
	session *clientSession
	stale   bool
}

// Client maintains a generation-aware business session. A failed business
// operation is never replayed; the next operation establishes a fresh session.
type Client struct {
	cfg ClientConfig

	mu         sync.Mutex
	current    *clientSession
	sessions   map[*clientSession]struct{}
	dialing    chan struct{}
	generation uint64
	operations sync.WaitGroup
	closing    bool
	closed     bool
	closeDone  chan struct{}
	closeErr   error
}

// NewClient connects and completes the exact v1 build handshake.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.WireBuild != WireBuild {
		return nil, fmt.Errorf("daemon: wire build %q, want exactly %q", cfg.WireBuild, WireBuild)
	}
	if cfg.Role == "" {
		return nil, errors.New("daemon: client role is required")
	}
	c := &Client{
		cfg:       cfg,
		sessions:  make(map[*clientSession]struct{}),
		closeDone: make(chan struct{}),
	}
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
	client, err := wire.NewClient(ctx, wireClientConfig(c.cfg))
	if err != nil {
		return nil, fmt.Errorf("daemon: connect: %w", err)
	}
	return &clientSession{wire: client}, nil
}

func wireClientConfig(cfg ClientConfig) wire.ClientConfig {
	maxFrame := cfg.MaxFrameBytes
	if maxFrame == 0 {
		maxFrame = maxFrameBytes
	}
	return wire.ClientConfig{
		Dial: wire.UnixDialer(cfg.Socket), WireBuild: cfg.WireBuild, Role: cfg.Role, MaxFrame: maxFrame,
	}
}

// Close permanently closes every business session.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closing || c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	c.closing = true
	c.mu.Unlock()

	c.operations.Wait()

	c.mu.Lock()
	c.closed = true
	sessions := make([]closingClientSession, 0, len(c.sessions))
	for session := range c.sessions {
		sessions = append(sessions, closingClientSession{session: session, stale: session.stale})
	}
	c.current = nil
	clear(c.sessions)
	c.mu.Unlock()
	var errs []error
	for _, closing := range sessions {
		if closing.stale {
			_ = closing.session.wire.Abort(errSessionRetired)
			continue
		}
		if err := closing.session.wire.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	c.mu.Lock()
	c.closeErr = err
	close(c.closeDone)
	c.mu.Unlock()
	return err
}

// Do dispatches one business operation without replaying it across sessions.
func (c *Client) Do(ctx context.Context, env Envelope) (Reply, error) {
	if err := c.beginOperation(); err != nil {
		return Reply{}, err
	}
	defer c.operations.Done()
	ctx, cancel := operationContext(ctx)
	defer cancel()
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

// RuntimeHealth returns the daemon's product-visible readiness snapshot.
func (c *Client) RuntimeHealth(ctx context.Context) (RuntimeHealth, error) {
	if err := c.beginOperation(); err != nil {
		return RuntimeHealth{}, err
	}
	defer c.operations.Done()
	ctx, cancel := operationContext(ctx)
	defer cancel()
	result, err := c.call(ctx, wire.Op(OpRuntimeHealth), nil)
	if err != nil {
		return RuntimeHealth{}, err
	}
	if result.Response.Err != "" {
		return RuntimeHealth{}, errors.New(result.Response.Err)
	}
	var health RuntimeHealth
	if err := decodeStrict(result.Response.Payload, &health); err != nil {
		return RuntimeHealth{}, fmt.Errorf("daemon: decode runtime health: %w", err)
	}
	return health, nil
}

func operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, handleTimeout+time.Second)
}

func (c *Client) beginOperation() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.closed {
		return ErrClientClosed
	}
	c.operations.Add(1)
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
		if callErr == nil {
			callErr = result.Rejection()
		}
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
		_ = session.wire.Abort(errSessionRetired)
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
