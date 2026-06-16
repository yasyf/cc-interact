package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/consume"
)

// watchConsumer is the stream-consumer name the watch command registers under,
// keeping its own resume cursor distinct from other consumers.
const watchConsumer = "watch"

// WatchCmd streams a subject's events as line-delimited JSON, one event per line,
// until a terminal event. It is meant to run under a Claude Code Monitor so each
// line becomes a chat notification the agent reacts to.
func WatchCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream subject events as line-delimited JSON (one event per line)",
		Long: "watch prints one JSON event per line as events arrive, then exits on the\n" +
			"terminal event. It is meant to run under a Claude Code Monitor so each line\n" +
			"becomes a chat notification and the agent reacts per event. Output is line-buffered\n" +
			"and resumes from a persisted cursor, so restarting it re-delivers nothing it\n" +
			"already emitted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := d.EnsureCurrent(ctx); err != nil {
				return err
			}
			client := d.NewClient()
			scope := mustCwd(cwd)
			claudePID := d.ClaudePID()
			subjectID, port, err := resolveSubject(ctx, client, session, scope, claudePID, watchConsumer)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			src := consume.StreamSource{
				Port: port, SubjectID: subjectID, Consumer: watchConsumer, ClaudePID: claudePID,
				Paths: d.Paths, WindowAlive: d.WindowAlive,
				Refresh: refreshHandshake(client, session, scope, claudePID, watchConsumer),
			}
			return consume.ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
				// A failed write must propagate so the cursor doesn't advance past
				// an undelivered event (at-least-once).
				if _, err := fmt.Fprintln(out, data); err != nil {
					return false, err
				}
				return d.TerminalEvent(eventType(data)), nil
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}
