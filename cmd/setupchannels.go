package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/channelsetup"
)

var managedSettingsPath = channelsetup.ManagedSettingsPath

type channelSetupState struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// SetupChannelsCmd builds the hidden channel-approval setup command.
func SetupChannelsCmd(d Deps, plugin channelsetup.Plugin, applyHint string) *cobra.Command {
	var check, apply, decline bool
	cmd := &cobra.Command{
		Use:    "setup-channels",
		Hidden: true,
		Short:  "Approve channel delivery",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case apply:
				return runChannelsApply(cmd.OutOrStdout(), d, plugin, applyHint)
			case decline:
				return writeChannelMarker(d, "declined")
			default:
				return runChannelsCheck(cmd.OutOrStdout(), d, plugin)
			}
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "print the first-run channel offer (default)")
	cmd.Flags().BoolVar(&apply, "apply", false, "approve channel delivery (prompts for admin)")
	cmd.Flags().BoolVar(&decline, "decline", false, "decline the channel delivery offer")
	cmd.MarkFlagsMutuallyExclusive("check", "apply", "decline")
	return cmd
}

func runChannelsCheck(out io.Writer, d Deps, plugin channelsetup.Plugin) error {
	managedPath, err := managedSettingsPath()
	if err != nil {
		return err
	}
	offer, reason, err := channelsetup.Offer(plugin, d.Paths.ChannelSetupMarkerPath(), managedPath)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(map[string]any{"offer": offer, "reason": reason}); err != nil {
		return fmt.Errorf("write channel offer: %w", err)
	}
	return nil
}

func runChannelsApply(out io.Writer, d Deps, plugin channelsetup.Plugin, applyHint string) error {
	managedPath, err := managedSettingsPath()
	if err != nil {
		return err
	}
	managed, err := readFileOrEmpty(managedPath)
	if err != nil {
		return err
	}
	merged, err := plugin.MergeManaged(managed)
	if err != nil {
		return err
	}
	if err := plugin.ApplyManagedViaAdmin(merged); err != nil {
		return err
	}
	if err := writeChannelMarker(d, "done"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, applyHint); err != nil {
		return fmt.Errorf("write channel setup hint: %w", err)
	}
	return nil
}

func writeChannelMarker(d Deps, status string) error {
	if err := d.Paths.EnsureStateDir(); err != nil {
		return fmt.Errorf("create app state dir: %w", err)
	}
	body, err := json.Marshal(channelSetupState{Status: status, Version: d.Version})
	if err != nil {
		return fmt.Errorf("encode channel marker: %w", err)
	}
	if err := os.WriteFile(d.Paths.ChannelSetupMarkerPath(), body, 0o600); err != nil {
		return fmt.Errorf("write channel marker: %w", err)
	}
	return nil
}

func readFileOrEmpty(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the tool-owned Claude managed settings file.
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return b, nil
}
