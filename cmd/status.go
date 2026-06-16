package cmd

import (
	"fmt"

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
			client := d.NewClient()
			if !client.Available() {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon: not running")
				return nil
			}
			// A running daemon gets the version handshake (and upgrade) like every
			// other op; a stopped one is just reported, not spawned.
			if err := d.EnsureCurrent(ctx); err != nil {
				return err
			}
			reply, err := client.Do(ctx, daemon.Envelope{
				Op: daemon.OpStatus, Session: session, ClaudePID: d.ClaudePID(), Scope: mustCwd(cwd),
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "daemon: running (%s)\n", reply.DaemonVersion)
			fmt.Fprintf(out, "http:   127.0.0.1:%d\n", reply.HTTPPort)
			if reply.SubjectID != "" {
				fmt.Fprintf(out, "subject: %s (%s)\n", reply.SubjectID, reply.Status)
			} else {
				fmt.Fprintln(out, "subject: none for this session/scope")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}

// StopCmd asks a running daemon to step down.
func StopCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := d.NewClient()
			if !client.Available() {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon: not running")
				return nil
			}
			if _, err := client.Shutdown(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon: stopping")
			return nil
		},
	}
}
