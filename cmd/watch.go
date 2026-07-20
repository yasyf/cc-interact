package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/event"
)

// watchConsumer is the base stream-consumer name for per-process watch cursors.
const watchConsumer = "watch"

// WatchCmd streams a subject's events as line-delimited JSON, one event per line,
// until a terminal event. It is meant to run under a Claude Code Monitor so each
// line becomes a chat notification the agent reacts to.
func WatchCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
		once    bool
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream subject events as line-delimited JSON (one event per line)",
		Long: "watch prints one JSON event per line as events arrive, then exits on the\n" +
			"terminal event. It is meant to run under a Claude Code Monitor so each line\n" +
			"becomes a chat notification and the agent reacts per event. Output is line-buffered\n" +
			"and resumes from a persisted cursor; a persistence failure warns without stopping\n" +
			"and may re-deliver after restart. With --once it exits after the first emitted event, so a\n" +
			"background task can relay one event and relaunch from the cursor.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := d.EnsureCurrent(ctx); err != nil {
				return err
			}
			client, err := d.NewClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			scope := mustCwd(cwd)
			claudePID := d.ClaudePID()
			consumer := fmt.Sprintf("%s-%d", watchConsumer, os.Getpid())
			subjectID, port, err := resolveSubject(ctx, client, session, scope, claudePID, consumer)
			if err != nil {
				return err
			}
			if err := consume.SeedCursor(d.Paths, subjectID, watchConsumer, consumer); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			src := consume.StreamSource{
				Port: port, SubjectID: subjectID, Consumer: consumer, ClaudePID: claudePID,
				ExcludeOrigin: event.OriginAgent,
				Paths:         d.Paths, WindowAlive: d.WindowAlive,
				Refresh: refreshHandshake(d.NewClient, session, scope, claudePID, consumer),
			}
			return consume.ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
				// A failed write must propagate so the cursor doesn't advance past
				// an undelivered event (at-least-once).
				if _, err := fmt.Fprintln(out, data); err != nil {
					return false, err
				}
				if once {
					return true, nil
				}
				return d.TerminalEvent(eventType(data)), nil
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	cmd.Flags().BoolVar(&once, "once", false, "exit after the first emitted event")
	return cmd
}
