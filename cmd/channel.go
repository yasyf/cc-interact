package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/cc-interact/procs"
)

// ChannelServer is the plugin.json name of a plugin's toolless channel server,
// the one ChannelCheckCmd reconnects.
const ChannelServer = "channel"

const (
	headRetryDelay = time.Second
	beatEvery      = 15 * time.Second
	upgradeBeats   = 4
	resumedFlag    = "resumed"
)

// ChannelCmd is the hidden, opt-in stdio MCP channel server. Loaded at session
// start, it advertises the consumer's domain tools and pushes each subject event
// down the same pipe as a JSON-RPC notification, so the agent reacts without
// polling while the loop answers tool calls. A window keeps one stream: a
// toolless server outranks one serving tools, and among one kind the
// later-started outranks the earlier. An outranked server serving tools keeps
// serving them; a toolless one exits.
//
// With --no-tools it serves only the stream, so a plugin starts it through a
// launcher that names no plugin version and execs the newest install's own
// binary, with CLAUDE_PLUGIN_ROOT emptied, a config a plugin reload keeps, and
// its tools on ToolsCmd's server. It stamps its window's beat for ChannelCheckCmd and
// re-executes on the newest install in Claude Code's plugin cache once that
// install runs a newer release.
func ChannelCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
		noTools bool
		resumed bool
	)
	cmd := &cobra.Command{
		Use:    "channel",
		Hidden: true,
		Short:  "Run the opt-in channel MCP server (stdio)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if session == "" {
				session = os.Getenv("CLAUDE_CODE_SESSION_ID")
			}
			scope := mustCwd(cwd)
			tools, notifyMethod, instructions, err := d.ChannelTools(ctx, session, scope)
			if err != nil {
				return err
			}
			if noTools {
				tools = nil
			}
			started, err := procs.StartTime(os.Getpid())
			if err != nil {
				return err
			}
			srv := channel.NewServer(channel.ServerInfo{Name: cmd.Root().Name(), Version: d.Version, Instructions: instructions}, tools)
			streamCtx, cancelStream := context.WithCancel(ctx)
			defer cancelStream()
			if noTools {
				go beat(streamCtx, d, srv, cmd.Root().Name(), cmd.ErrOrStderr())
			}
			streamed := make(chan error, 1)
			go func() {
				streamed <- streamToChannel(streamCtx, d, srv, session, scope, notifyMethod, started, noTools, resumed)
			}()
			served := make(chan error, 1)
			go func() { served <- srv.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout()) }()
			select {
			case err := <-served:
				cancelStream()
				<-streamed
				return err
			case err := <-streamed:
				if noTools && errors.Is(err, consume.ErrSuperseded) {
					return nil
				}
				return <-served
			}
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	cmd.Flags().BoolVar(&noTools, "no-tools", false, "serve only the channel stream; the tools command serves the tools")
	cmd.Flags().BoolVar(&resumed, resumedFlag, false, "keep the stream position of the channel server this process re-executed")
	_ = cmd.Flags().MarkHidden(resumedFlag)
	return cmd
}

// ToolsCmd is the hidden stdio MCP server for the consumer's channel tools
// without the stream, the server a plugin pairs with ChannelCmd's --no-tools.
func ToolsCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
	)
	cmd := &cobra.Command{
		Use:    "tools",
		Hidden: true,
		Short:  "Run the channel's tools as an MCP server (stdio)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if session == "" {
				session = os.Getenv("CLAUDE_CODE_SESSION_ID")
			}
			tools, _, _, err := d.ChannelTools(cmd.Context(), session, mustCwd(cwd))
			if err != nil {
				return err
			}
			srv := channel.NewServer(channel.ServerInfo{Name: cmd.Root().Name(), Version: d.Version}, tools)
			return srv.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}

func beat(ctx context.Context, d Deps, srv *channel.Server, bin string, warn io.Writer) {
	pid := d.ClaudePID()
	for n := 1; ; n++ {
		if pid != 0 {
			if err := touch(statepath.ChannelBeat(d.Paths, pid)); err != nil {
				_, _ = fmt.Fprintln(warn, "channel beat:", err)
			}
		}
		if n%upgradeBeats == 0 {
			if path, ok := newerInstall(ctx, d, bin); ok {
				argv := slices.Concat([]string{path}, os.Args[1:])
				if !slices.Contains(argv, "--"+resumedFlag) {
					argv = append(argv, "--"+resumedFlag)
				}
				_, _ = fmt.Fprintf(warn, "channel: re-execute on %s: %v\n", path, srv.Exec(path, argv))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(beatEvery):
		}
	}
}

// newerInstall returns the binary of the newest install of d.Plugin in Claude
// Code's plugin cache that Claude Code has not marked orphaned, when it reports
// a newer release than this binary. Running it for its version also warms
// binrun's cache, so the re-executed process writes only JSON-RPC to stdout.
func newerInstall(ctx context.Context, d Deps, bin string) (string, bool) {
	running, ok := release(d.Version)
	if !ok || d.Plugin.Name == "" {
		return "", false
	}
	root := filepath.Join(pluginCache(), d.Plugin.Marketplace, d.Plugin.Name)
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	newest, dir := running, ""
	for _, e := range entries {
		v, ok := release(e.Name())
		if !ok || semver.Compare(v, newest) <= 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), ".orphaned_at")); err == nil {
			continue
		}
		newest, dir = v, e.Name()
	}
	if dir == "" {
		return "", false
	}
	path := filepath.Join(root, dir, "bin", bin)
	out, err := exec.CommandContext(ctx, path, "--version").Output() //nolint:gosec // G204: this plugin's own binary in Claude Code's plugin cache.
	fields := strings.Fields(string(out))
	if err != nil || len(fields) == 0 {
		return "", false
	}
	v, ok := release(fields[len(fields)-1])
	return path, ok && semver.Compare(v, running) > 0
}

func pluginCache() string {
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		home, _ := os.UserHomeDir()
		config = filepath.Join(home, ".claude")
	}
	return filepath.Join(config, "plugins", "cache")
}

func release(version string) (string, bool) {
	v := "v" + strings.TrimPrefix(version, "v")
	return v, semver.IsValid(v) && semver.Canonical(v) == v && semver.Prerelease(v) == ""
}

// streamToChannel waits for the daemon + subject, then pushes each event
// Deps.ChannelEvent admits as a channel notification for the lifetime of the
// session. The window pid is
// resolved once at boot — the channel server lives as long as the window — and
// keys the stream even when the session id is stale or unset. A resume moves the
// stream onto the session's subject, so it resolves again when told to. It
// returns consume.ErrSuperseded once a channel server that outranks this one
// holds the window's stream. A resumed server keeps its first subject's cursor
// where the process it re-executed left it.
func streamToChannel(ctx context.Context, d Deps, srv *channel.Server, session, scope, notifyMethod string, started int64, toolless, resumed bool) error {
	claudePID := d.ClaudePID()
	connect := reuseIdentity(d.NewClient)
	for {
		subjectID, port := waitForSubject(ctx, connect, session, scope, claudePID, daemon.ChannelConsumer)
		if subjectID == "" {
			return nil
		}
		if d.ChannelResumesLive && !resumed {
			head, err := consume.StreamHead(ctx, port, subjectID, event.OriginAgent)
			if err != nil {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(headRetryDelay):
				}
				continue
			}
			if err := consume.RaiseCursor(d.Paths, subjectID, daemon.ChannelConsumer, head); err != nil {
				return err
			}
		}
		resumed = false
		src := consume.StreamSource{
			Port: port, SubjectID: subjectID, Consumer: daemon.ChannelConsumer, ClaudePID: claudePID,
			Started: started, Toolless: toolless, ExcludeOrigin: event.OriginAgent,
			Paths: d.Paths, WindowAlive: d.WindowAlive,
			Refresh: refreshHandshake(connect, session, scope, claudePID, daemon.ChannelConsumer),
		}
		// No push at attach: an unsolicited tag wakes an idle agent for nothing. The
		// channel stays silent until the subject produces an event — or the daemon
		// injects a solicited frame (a delivery probe) into this stream.
		// A failed push propagates so the cursor doesn't advance past an undelivered
		// event; a channel otherwise runs for the whole session.
		err := channel.StreamEvents(ctx, src, func(eventType, data string) error {
			if d.ChannelEvent != nil && !d.ChannelEvent(eventType) {
				return nil
			}
			return srv.Notify(notifyMethod, map[string]any{
				"content": data,
				"meta":    map[string]any{"type": eventType, "subject_id": subjectID},
			})
		})
		if !errors.Is(err, channel.ErrWindowMoved) {
			return err
		}
	}
}
