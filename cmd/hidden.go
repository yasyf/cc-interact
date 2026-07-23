package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/wire"
)

// DaemonCmd is the hidden entry point the lazy-start spawns; it runs the
// long-lived daemon until its context is cancelled.
func DaemonCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Short:  "Run the background daemon",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Detached child (proc.Spawn contract): sweep inherited non-CLOEXEC fds
			// before opening anything, so no parent lease fd stays pinned for life.
			if err := proc.CloseInheritedFDs(); err != nil {
				return err
			}
			return d.Serve(cmd.Context())
		},
	}
}

// DaemonStopControlCmd is the reserved exact-role child entry point for stop.
func DaemonStopControlCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    daemon.StopControlCommand,
		Short:  "Stop the background daemon as its exact runtime role",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return d.RunStopControl(cmd.Context())
		},
	}
}

// SessionRecordCmd is the hidden SessionStart hook handler: it rebinds the
// window's subject to the rotated session id. Claude Code fires SessionStart
// before any tool use in the new session, so the rebind lands before the first
// guard-edit. Best-effort: with no daemon running there is nothing to rebind
// (resolution happens later regardless), and a hook must never boot one.
func SessionRecordCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "session-record",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := readHookInput(cmd.InOrStdin())
			ctx := cmd.Context()
			if err := d.EnsureCurrentIfRunning(ctx); err != nil {
				return nil
			}
			client, err := d.NewClient(ctx)
			if err != nil {
				return nil
			}
			defer func() { _ = client.Close() }()
			_, _ = client.Do(ctx, daemon.Envelope{
				Op: daemon.OpSessionRecord, Session: in.SessionID, ClaudePID: d.ClaudePID(), Scope: in.Cwd,
			})
			return nil
		},
	}
}

// ChannelAckCmd is the hidden command the model runs when the first channel tag
// arrives while its Monitor is armed: it proves the window's channel round trip,
// flipping later starts from pending to active.
func ChannelAckCmd(d Deps) *cobra.Command {
	var (
		session string
		cwd     string
	)
	cmd := &cobra.Command{
		Use:    "channel-ack",
		Hidden: true,
		Args:   cobra.NoArgs,
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
			reply, err := client.Do(ctx, daemon.Envelope{
				Op: daemon.OpChannelAck, Session: session, ClaudePID: d.ClaudePID(), Scope: mustCwd(cwd),
			})
			if err != nil {
				return err
			}
			if !reply.OK {
				return errors.New(reply.Error)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Claude session id (keys the subject with the scope)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (defaults to the current directory)")
	return cmd
}

// GuardEditCmd is the hidden PreToolUse(Edit|Write|NotebookEdit) hook handler.
// It denies edits while the gate refuses by writing the reason to stderr and
// exiting 2 — Claude Code's PreToolUse block signal — and fails open (exit 0)
// on daemon errors. Oversized request frames are logged before allowing the edit.
func GuardEditCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "guard-edit",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := readHookInput(cmd.InOrStdin())
			body, _ := json.Marshal(guardEditBody{ToolName: in.ToolName, ToolInput: in.ToolInput})
			env := daemon.Envelope{
				Op: daemon.OpGuardEdit, Session: in.SessionID, ClaudePID: d.ClaudePID(), Scope: in.Cwd, Body: body,
			}
			client, err := d.NewClient(cmd.Context())
			if err != nil {
				return nil // daemon down: nothing to guard
			}
			defer func() { _ = client.Close() }()
			reply, err := client.Do(cmd.Context(), env)
			if errors.Is(err, wire.ErrFrameTooLarge) {
				frame, _ := json.Marshal(env)
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "guard-edit: frame-too-large: request frame is %d bytes; allowing edit\n", len(frame))
				return nil
			}
			if err != nil {
				return nil
			}
			if reply.OK && !reply.Allow {
				fmt.Fprintln(os.Stderr, reply.Reason)
				os.Exit(2)
			}
			return nil
		},
	}
}

// guardEditBody is the OpGuardEdit envelope payload: the tool name and its raw,
// un-interpreted input, matching the daemon's guard-edit handler.
type guardEditBody struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}
