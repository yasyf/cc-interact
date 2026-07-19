package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-interact/channelsetup"
	"github.com/yasyf/daemonkit/paths"
)

func TestSetupChannelsCheck(t *testing.T) {
	plugin := channelsetup.Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}
	for _, tc := range []struct {
		name       string
		args       []string
		marker     bool
		wantOutput string
	}{
		{name: "offer with default check", wantOutput: `{"offer":true,"reason":"channel not yet approved"}` + "\n"},
		{name: "already offered with explicit check", args: []string{"--check"}, marker: true, wantOutput: `{"offer":false,"reason":"already offered"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			setManagedSettingsPath(t, filepath.Join(t.TempDir(), "managed-settings.json"))
			d := Deps{Paths: paths.Paths{App: ".cc-test"}, Version: "v1.2.3"}
			if tc.marker {
				if err := d.Paths.EnsureStateDir(); err != nil {
					t.Fatalf("ensure state dir: %v", err)
				}
				if err := os.WriteFile(d.Paths.ChannelSetupMarkerPath(), []byte(`{"status":"declined","version":"v1.2.3"}`), 0o600); err != nil {
					t.Fatalf("write marker: %v", err)
				}
			}
			var out bytes.Buffer
			cmd := SetupChannelsCmd(d, plugin, "enabled")
			cmd.SetOut(&out)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute setup-channels: %v", err)
			}
			if got := out.String(); got != tc.wantOutput {
				t.Errorf("output = %q, want %q", got, tc.wantOutput)
			}
		})
	}
}

func TestSetupChannelsDeclineThenCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setManagedSettingsPath(t, filepath.Join(t.TempDir(), "managed-settings.json"))
	d := Deps{Paths: paths.Paths{App: ".cc-test"}, Version: "v1.2.3"}
	plugin := channelsetup.Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}

	decline := SetupChannelsCmd(d, plugin, "enabled")
	decline.SetOut(&bytes.Buffer{})
	decline.SetArgs([]string{"--decline"})
	if err := decline.Execute(); err != nil {
		t.Fatalf("execute setup-channels --decline: %v", err)
	}
	body, err := os.ReadFile(d.Paths.ChannelSetupMarkerPath())
	if err != nil {
		t.Fatalf("read channel marker: %v", err)
	}
	var marker channelSetupState
	if err := json.Unmarshal(body, &marker); err != nil {
		t.Fatalf("decode channel marker: %v", err)
	}
	if marker.Status != "declined" || marker.Version != d.Version {
		t.Errorf("marker = %+v, want status declined and version %q", marker, d.Version)
	}

	var out bytes.Buffer
	check := SetupChannelsCmd(d, plugin, "enabled")
	check.SetOut(&out)
	if err := check.Execute(); err != nil {
		t.Fatalf("execute following setup-channels check: %v", err)
	}
	want := `{"offer":false,"reason":"already offered"}` + "\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestSetupChannelsFlagsMutuallyExclusive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setManagedSettingsPath(t, filepath.Join(t.TempDir(), "managed-settings.json"))
	d := Deps{Paths: paths.Paths{App: ".cc-test"}, Version: "v1.2.3"}
	plugin := channelsetup.Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}
	cmd := SetupChannelsCmd(d, plugin, "enabled")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--apply", "--decline"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("execute setup-channels --apply --decline: want error, got nil")
	}
}

func setManagedSettingsPath(t *testing.T, path string) {
	t.Helper()
	old := managedSettingsPath
	managedSettingsPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { managedSettingsPath = old })
}
