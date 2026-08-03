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

	"github.com/yasyf/daemonkit"
)

// ErrMalformedReply means the daemon delivered a reply this build cannot
// decode — schema drift, not transport loss.
var ErrMalformedReply = errors.New("daemon: malformed reply")

// Client is one daemon's business lane: unary, concurrency-safe, never
// replaying. Session establishment, retirement, and trust verification are
// daemonkit's; this type owns only the Envelope/Reply encoding.
type Client struct {
	business *daemonkit.Business

	closeMu  sync.Mutex
	closed   bool
	closeErr error
}

// NewClient opens the business lane of the daemon d names. The lane
// authenticates the accepting process against d.Trust.Serving on every
// session acquisition; the first Do reports a provably absent daemon as
// daemonkit.ErrAbsent.
func NewClient(d daemonkit.Daemon) (*Client, error) {
	if err := validateSchemas(d); err != nil {
		return nil, err
	}
	dk, err := daemonkit.Open(d)
	if err != nil {
		return nil, err
	}
	return &Client{business: dk.Business()}, nil
}

// Close releases the business lane; every later Do is refused. The first
// close is the terminal one and every later call replays its outcome, so a
// deferred close behind an explicit one reports the same failure rather than
// the success of closing an already-released lane.
func (c *Client) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return c.closeErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.closed, c.closeErr = true, c.business.Close(ctx)
	return c.closeErr
}

// Do dispatches one business operation without replaying it across sessions.
func (c *Client) Do(ctx context.Context, env Envelope) (Reply, error) {
	ctx, cancel := operationContext(ctx)
	defer cancel()
	payload, err := json.Marshal(env)
	if err != nil {
		return Reply{}, fmt.Errorf("daemon: encode %s: %w", env.Op, err)
	}
	result, err := c.business.Call(ctx, string(env.Op), payload)
	if err != nil {
		return Reply{}, err
	}
	var reply Reply
	if err := decodeStrict(result.Body, &reply); err != nil {
		return Reply{}, fmt.Errorf("%w: decode %s response: %w", ErrMalformedReply, env.Op, err)
	}
	return reply, nil
}

func operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, handleTimeout+time.Second)
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
