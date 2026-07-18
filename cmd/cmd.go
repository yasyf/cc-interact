// Package cmd provides reusable cobra command constructors for cc-interact's
// substrate: the long-lived daemon, the SSE watch consumer, status/stop control,
// and the hidden Claude Code hook + channel entry points. Each constructor takes
// a Deps so the consumer composes its own command tree and layers domain
// commands on top; the substrate carries no domain concepts. Every command is a
// thin shell around the daemon control client and the stream consumer.
package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/paths"
)

// Deps is the host wiring every substrate command shares. The consumer builds it
// once and passes it to each constructor; the closures bridge to the host's own
// daemon launcher, control client, window identity, and domain channel tools.
type Deps struct {
	// Paths is the state-directory layout, used to locate per-consumer SSE cursors.
	Paths paths.Paths
	// Version is this binary's build version, advertised in the channel server's
	// initialize handshake.
	Version string
	// NewClient returns a control-socket client (typically
	// daemon.NewClient(Paths.SocketPath())).
	NewClient func() *daemon.Client
	// EnsureCurrent lazily starts or upgrades the daemon, blocking until a
	// current one answers (daemon.Launcher.EnsureCurrent).
	EnsureCurrent func(ctx context.Context) error
	// EnsureCurrentIfRunning upgrades a running daemon but never cold-starts one —
	// for hooks, which must not boot daemons (daemon.Launcher.EnsureCurrentIfRunning).
	EnsureCurrentIfRunning func() error
	// ClaudePID resolves the window identity stamped on every envelope (typically
	// procs.ClaudePID). 0 is a pid-less consumer outside any Claude window.
	ClaudePID func() int
	// WindowAlive reports whether a window pid still lives (typically
	// procs.LiveClaude). A pid-bound stream consumer exits once its window dies;
	// nil means consumers never self-terminate on window death.
	WindowAlive func(pid int) bool
	// TerminalEvent reports whether an event type ends a watch (cc-review: "submit").
	TerminalEvent func(eventType string) bool
	// Serve runs the long-lived daemon (consumer builds daemon.New(Config).Serve).
	Serve func(ctx context.Context) error
	// ChannelTools supplies the channel server's domain tools, the JSON-RPC method
	// each subject event is pushed under, and optional instructions folded into the
	// agent's prompt at initialize, built against the resolved session and scope
	// (cc-review supplies its review tools and channel guidance).
	ChannelTools func(ctx context.Context, session, scope string) (tools []channel.Tool, notifyMethod, instructions string, err error)
}

// hookInput is the subset of a Claude Code hook's stdin JSON the substrate hooks
// read: the session identity, the working directory (the raw ownership scope),
// and the PreToolUse tool call.
type hookInput struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// readHookInput parses a hook's stdin JSON, tolerating an empty body.
func readHookInput(r io.Reader) hookInput {
	b, err := io.ReadAll(r)
	if err != nil || len(b) == 0 {
		return hookInput{}
	}
	var in hookInput
	_ = json.Unmarshal(b, &in)
	return in
}

// mustCwd returns cwd, defaulting to the process working directory.
func mustCwd(cwd string) string {
	if cwd != "" {
		return cwd
	}
	d, _ := os.Getwd()
	return d
}
