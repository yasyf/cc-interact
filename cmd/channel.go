package cmd

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

// ChannelCmd is the hidden, opt-in stdio MCP channel server. Loaded at session
// start, it advertises the consumer's domain tools and pushes each subject event
// down the same pipe as a JSON-RPC notification, so the agent reacts without
// polling while the loop answers tool calls.
func ChannelCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
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
			srv := channel.NewServer(channel.ServerInfo{Name: cmd.Root().Name(), Version: d.Version, Instructions: instructions}, tools)
			streamCtx, cancelStream := context.WithCancel(ctx)
			streamDone := make(chan struct{})
			go func() {
				defer close(streamDone)
				streamToChannel(streamCtx, d, srv, session, scope, notifyMethod)
			}()
			err = srv.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
			cancelStream()
			<-streamDone
			return err
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}

// streamToChannel waits for the daemon + subject, then pushes each event
// Deps.ChannelEvent admits as a channel notification for the lifetime of the
// session. The window pid is
// resolved once at boot — the channel server lives as long as the window — and
// keys the stream even when the session id is stale or unset. A resume moves the
// stream onto the session's subject, so it resolves again when told to.
func streamToChannel(ctx context.Context, d Deps, srv *channel.Server, session, scope, notifyMethod string) {
	claudePID := d.ClaudePID()
	connect := reuseIdentity(d.NewClient)
	for {
		subjectID, port := waitForSubject(ctx, connect, session, scope, claudePID, daemon.ChannelConsumer)
		if subjectID == "" {
			return
		}
		src := consume.StreamSource{
			Port: port, SubjectID: subjectID, Consumer: daemon.ChannelConsumer, ClaudePID: claudePID,
			ExcludeOrigin: event.OriginAgent,
			Paths:         d.Paths, WindowAlive: d.WindowAlive,
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
			return
		}
	}
}
