package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/daemon"
)

// StatusCmd reports daemon liveness and the subject bound to the session+scope.
func StatusCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon and subject status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			switch err := d.EnsureCurrentIfRunning(ctx); {
			case errors.Is(err, daemon.ErrNoPeer):
				fmt.Fprintln(cmd.OutOrStdout(), "daemon: not running")
				return nil
			case errors.Is(err, daemon.ErrLegacyDaemon):
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"daemon: a pre-0.21 daemon is running and cannot be upgraded in place; "+
						"run %q, then any command that starts it\n",
					cmd.Root().Name()+" stop")
				return nil
			case err != nil:
				return err
			}
			client, err := d.NewClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			reply, err := client.Do(ctx, daemon.Envelope{
				Op: daemon.OpStatus, Session: session, ClaudePID: d.ClaudePID(), Scope: mustCwd(cwd),
			})
			if err != nil {
				return err
			}
			var body daemon.StatusBody
			if len(reply.Body) > 0 {
				if err := json.Unmarshal(reply.Body, &body); err != nil {
					return fmt.Errorf("decode status body: %w", err)
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "daemon: running (%s)\n", reply.DaemonVersion)
			fmt.Fprintf(out, "http:   127.0.0.1:%d\n", reply.HTTPPort)
			if reply.SubjectID != "" {
				fmt.Fprintf(out, "subject: %s (%s)\n", reply.SubjectID, reply.Status)
			} else {
				fmt.Fprintln(out, "subject: none for this session/scope")
			}
			if len(reply.Body) > 0 {
				watchers := 0
				for consumer, count := range body.Consumers {
					if consumer == watchConsumer || strings.HasPrefix(consumer, watchConsumer+"-") {
						watchers += count
					}
				}
				_, _ = fmt.Fprintf(out, "watchers: %d\n", watchers)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}

// StopCmd stops the background daemon and takes down its LaunchAgent. The
// postcondition is uniform — nothing serving, no agent — so a daemon that was
// already stopped reports the same success, having still had any agent it left
// behind removed.
func StopCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := d.Stop(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon: stopped")
			return nil
		},
	}
}
