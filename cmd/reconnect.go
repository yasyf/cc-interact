package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/cc-interact/procs"
)

const (
	channelStale   = time.Minute
	reconnectEvery = 5 * time.Minute
	reconnectTries = 6
	reconnectPause = 20 * time.Second
	idleWaitMS     = 30_000
)

type orcaRunner func(ctx context.Context, args ...string) (string, error)

// ChannelCheckCmd is the hidden Stop hook that brings back a toolless channel
// server that stopped. Claude Code never restarts a stdio server that exits, and
// only /mcp reconnect typed at the prompt starts one again. In a session inside
// Orca whose channel server beat since the window started and then went quiet,
// it starts a detached copy of itself, at most once per reconnectEvery, that
// waits for the session to sit idle with an empty draft and types the
// reconnect.
func ChannelCheckCmd(d Deps) *cobra.Command {
	var window int
	cmd := &cobra.Command{
		Use:    "channel-check",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			terminal := os.Getenv("ORCA_TERMINAL_HANDLE")
			if terminal == "" {
				return nil
			}
			pid := window
			if pid == 0 {
				pid = d.ClaudePID()
			}
			if pid == 0 {
				return nil
			}
			started, err := procs.StartTime(pid)
			if err != nil {
				return err
			}
			beat := statepath.ChannelBeat(d.Paths, pid)
			if window != 0 {
				return typeReconnect(cmd.Context(), orca, terminal, "/mcp reconnect "+d.Plugin.Source(ChannelServer),
					func() bool { return channelDown(beat, started, time.Now()) }, func() { time.Sleep(reconnectPause) })
			}
			if !channelDown(beat, started, time.Now()) || !claimReconnect(statepath.ChannelReconnect(d.Paths, pid), started, time.Now()) {
				return nil
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			typer := exec.Command(self, slices.Concat(os.Args[1:], []string{"--window", strconv.Itoa(pid)})...) //nolint:gosec // G204: this binary, run again for its own window.
			typer.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			return typer.Start()
		},
	}
	cmd.Flags().IntVar(&window, "window", 0, "type the reconnect for this window's claude pid")
	_ = cmd.Flags().MarkHidden("window")
	return cmd
}

// channelDown reports whether the channel server beating at beat ran since its
// window started, in Unix milliseconds, and has not beat for channelStale.
func channelDown(beat string, windowStarted int64, now time.Time) bool {
	info, err := os.Stat(beat)
	return err == nil && info.ModTime().UnixMilli() >= windowStarted && now.Sub(info.ModTime()) > channelStale
}

// claimReconnect stamps path and reports true unless a reconnect started there
// within reconnectEvery since the window started.
func claimReconnect(path string, windowStarted int64, now time.Time) bool {
	if info, err := os.Stat(path); err == nil && info.ModTime().UnixMilli() >= windowStarted && now.Sub(info.ModTime()) < reconnectEvery {
		return false
	}
	return touch(path) == nil
}

// typeReconnect types command into terminal once it sits idle with nothing in
// its input box, so it never lands in a human's half-written prompt, while down
// still holds. It gives up after reconnectTries; a later Stop starts another.
func typeReconnect(ctx context.Context, orca orcaRunner, terminal, command string, down func() bool, pause func()) error {
	for range reconnectTries {
		if !down() {
			return nil
		}
		typed, err := typeIfIdle(ctx, orca, terminal, command)
		if typed || err != nil {
			return err
		}
		pause()
	}
	return nil
}

func typeIfIdle(ctx context.Context, orca orcaRunner, terminal, command string) (bool, error) {
	if _, err := orca(ctx, "terminal", "wait", "--terminal", terminal, "--for", "tui-idle", "--timeout-ms", strconv.Itoa(idleWaitMS)); err != nil {
		return false, nil //nolint:nilerr // a busy session times the wait out; the next try waits again.
	}
	out, err := orca(ctx, "terminal", "read", "--terminal", terminal, "--screen", "--json")
	if err != nil {
		return false, err
	}
	var read struct {
		Result struct {
			Terminal struct {
				Draft  json.RawMessage `json:"draft"`
				Source string          `json:"source"`
			} `json:"terminal"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &read); err != nil {
		return false, fmt.Errorf("decode terminal read: %w", err)
	}
	switch string(read.Result.Terminal.Draft) {
	case "", "null", "false", `""`:
	default:
		return false, nil
	}
	if read.Result.Terminal.Source != "screen" {
		return false, nil
	}
	_, err = orca(ctx, "terminal", "send", "--terminal", terminal, "--text", command, "--enter")
	return err == nil, err
}

func orca(ctx context.Context, args ...string) (string, error) {
	bin := "orca"
	if dir := os.Getenv("ORCA_CLI_BIN_DIR"); dir != "" {
		bin = filepath.Join(dir, "orca")
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output() //nolint:gosec // G204: the Orca CLI the session's terminal announced.
	return string(out), err
}

func touch(path string) error {
	now := time.Now()
	err := os.Chtimes(path, now, now)
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}
