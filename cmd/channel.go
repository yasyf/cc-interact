package cmd

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/consume"
)

// channelConsumer is the stream-consumer name the channel server registers under.
const channelConsumer = "channel"

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
			tools, notifyMethod, err := d.ChannelTools(ctx, session, scope)
			if err != nil {
				return err
			}
			srv := channel.NewServer(channel.ServerInfo{Name: cmd.Root().Name(), Version: d.Version}, tools)
			go streamToChannel(ctx, d, srv, session, scope, notifyMethod)
			return srv.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}

// streamToChannel waits for the daemon + subject, then pushes every event as a
// channel notification for the lifetime of the session. The window pid is
// resolved once at boot — the channel server lives as long as the window — and
// keys the stream even when the session id is stale or unset.
func streamToChannel(ctx context.Context, d Deps, srv *channel.Server, session, scope, notifyMethod string) {
	client := d.NewClient()
	claudePID := d.ClaudePID()
	subjectID, port := waitForSubject(ctx, client, session, scope, claudePID, channelConsumer)
	if subjectID == "" {
		return
	}
	src := consume.StreamSource{
		Port: port, SubjectID: subjectID, Consumer: channelConsumer, ClaudePID: claudePID,
		Paths: d.Paths, Refresh: refreshHandshake(client, session, scope, claudePID, channelConsumer),
	}
	// A failed push propagates so the cursor doesn't advance past an undelivered
	// event; a channel otherwise runs for the whole session.
	_ = channel.StreamEvents(ctx, src, func(eventType, data string) error {
		return srv.Notify(notifyMethod, map[string]any{
			"content": data,
			"meta":    map[string]any{"type": eventType, "subject_id": subjectID},
		})
	})
}
