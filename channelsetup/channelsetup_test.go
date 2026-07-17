package channelsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPluginIdentity(t *testing.T) {
	for _, tc := range []struct {
		plugin     Plugin
		server     string
		wantID     string
		wantSource string
	}{
		{
			plugin:     Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"},
			server:     "cc-orchestrate",
			wantID:     "plugin:cc-orchestrate@cc-orchestrate",
			wantSource: "plugin:cc-orchestrate:cc-orchestrate",
		},
		{
			plugin:     Plugin{Marketplace: "cc-review", Name: "cc-review"},
			server:     "cc-review",
			wantID:     "plugin:cc-review@cc-review",
			wantSource: "plugin:cc-review:cc-review",
		},
		{
			plugin:     Plugin{Marketplace: "market", Name: "plug"},
			server:     "srv",
			wantID:     "plugin:plug@market",
			wantSource: "plugin:plug:srv",
		},
	} {
		if got := tc.plugin.ChannelID(); got != tc.wantID {
			t.Errorf("ChannelID() = %q, want %q", got, tc.wantID)
		}
		if got := tc.plugin.Source(tc.server); got != tc.wantSource {
			t.Errorf("Source(%q) = %q, want %q", tc.server, got, tc.wantSource)
		}
	}
}

func TestMergeManaged(t *testing.T) {
	plugin := Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}
	tests := []struct {
		name        string
		existing    string
		wantOther   bool
		wantDiscord bool
	}{
		{name: "empty"},
		{name: "preserves unrelated keys", existing: `{"otherKey":"keep","permissions":{"allow":["Bash"]}}`, wantOther: true},
		{name: "already approved", existing: `{"channelsEnabled":true,"allowedChannelPlugins":[{"marketplace":"cc-orchestrate","plugin":"cc-orchestrate"}]}`},
		{name: "appends beside another plugin", existing: `{"allowedChannelPlugins":[{"marketplace":"claude-plugins-official","plugin":"discord"}]}`, wantDiscord: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := plugin.MergeManaged([]byte(tc.existing))
			if err != nil {
				t.Fatalf("MergeManaged: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(merged, &got); err != nil {
				t.Fatalf("decode merged settings: %v", err)
			}
			if got["channelsEnabled"] != true {
				t.Errorf("channelsEnabled = %v, want true", got["channelsEnabled"])
			}
			has, err := plugin.ManagedHasEntry(merged)
			if err != nil {
				t.Fatalf("ManagedHasEntry: %v", err)
			}
			if !has {
				t.Errorf("merged settings missing plugin entry: %s", merged)
			}
			list, _ := got["allowedChannelPlugins"].([]any)
			if countEntry(list, plugin.Marketplace, plugin.Name) != 1 {
				t.Errorf("plugin entry count = %d, want 1", countEntry(list, plugin.Marketplace, plugin.Name))
			}
			if tc.wantOther && got["otherKey"] != "keep" {
				t.Errorf("otherKey = %v, want keep", got["otherKey"])
			}
			if tc.wantDiscord && countEntry(list, "claude-plugins-official", "discord") != 1 {
				t.Errorf("discord entry was not preserved: %s", merged)
			}
			twice, err := plugin.MergeManaged(merged)
			if err != nil {
				t.Fatalf("second MergeManaged: %v", err)
			}
			if string(twice) != string(merged) {
				t.Errorf("merge is not idempotent:\nfirst=%s\nsecond=%s", merged, twice)
			}
		})
	}
}

func TestMergeManagedRejectsInvalidObject(t *testing.T) {
	plugin := Plugin{Marketplace: "market", Name: "plug"}
	for _, tc := range []struct {
		name     string
		existing string
	}{
		{name: "malformed JSON", existing: `{not json`},
		{name: "null document", existing: `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := plugin.MergeManaged([]byte(tc.existing)); err == nil {
				t.Fatal("MergeManaged succeeded, want error")
			}
		})
	}
}

func TestManagedHasEntry(t *testing.T) {
	plugin := Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}
	tests := []struct {
		name     string
		existing string
		want     bool
	}{
		{name: "missing", existing: `{}`},
		{name: "entry present and channels disabled", existing: `{"channelsEnabled":false,"allowedChannelPlugins":[{"marketplace":"cc-orchestrate","plugin":"cc-orchestrate"}]}`},
		{name: "channels enabled and entry missing", existing: `{"channelsEnabled":true,"allowedChannelPlugins":[]}`},
		{name: "channels enabled and entry present", existing: `{"channelsEnabled":true,"allowedChannelPlugins":[{"marketplace":"cc-orchestrate","plugin":"cc-orchestrate"}]}`, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := plugin.ManagedHasEntry([]byte(tc.existing))
			if err != nil {
				t.Fatalf("ManagedHasEntry: %v", err)
			}
			if got != tc.want {
				t.Errorf("ManagedHasEntry = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAllowlistWrongTypeErrors(t *testing.T) {
	plugin := Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}
	bad := []byte(`{"allowedChannelPlugins":"not-a-list"}`)
	if _, err := plugin.MergeManaged(bad); err == nil {
		t.Error("MergeManaged on non-array allowlist: want error, got nil")
	}
	if _, err := plugin.ManagedHasEntry(bad); err == nil {
		t.Error("ManagedHasEntry on non-array allowlist: want error, got nil")
	}
}

func TestAdminScriptQuoting(t *testing.T) {
	got := adminScript("/L S/managed.json", `/t'mp/f".json`)
	want := `do shell script "mkdir -p '/L S' && cp '/t'\\''mp/f\".json' '/L S/managed.json' && chmod 644 '/L S/managed.json'" with administrator privileges`
	if got != want {
		t.Fatalf("adminScript =\n%s\nwant\n%s", got, want)
	}
}

func TestOffer(t *testing.T) {
	plugin := Plugin{Marketplace: "cc-orchestrate", Name: "cc-orchestrate"}
	tests := []struct {
		name       string
		marker     bool
		managed    string
		wantOffer  bool
		wantReason string
	}{
		{name: "marker exists", marker: true, wantReason: "already offered"},
		{name: "managed settings approved", managed: `{"channelsEnabled":true,"allowedChannelPlugins":[{"marketplace":"cc-orchestrate","plugin":"cc-orchestrate"}]}`, wantReason: "already approved"},
		{name: "not yet approved", wantOffer: true, wantReason: "channel not yet approved"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			markerPath := filepath.Join(dir, "channels-setup.json")
			managedPath := filepath.Join(dir, "managed-settings.json")
			if tc.marker {
				if err := os.WriteFile(markerPath, []byte(`{"status":"declined","version":"test"}`), 0o600); err != nil {
					t.Fatalf("write marker: %v", err)
				}
			}
			if tc.managed != "" {
				if err := os.WriteFile(managedPath, []byte(tc.managed), 0o600); err != nil {
					t.Fatalf("write managed settings: %v", err)
				}
			}
			gotOffer, gotReason, err := Offer(plugin, markerPath, managedPath)
			if err != nil {
				t.Fatalf("Offer: %v", err)
			}
			if gotOffer != tc.wantOffer || gotReason != tc.wantReason {
				t.Errorf("Offer = (%t, %q), want (%t, %q)", gotOffer, gotReason, tc.wantOffer, tc.wantReason)
			}
		})
	}
}

func countEntry(list []any, marketplace, plugin string) int {
	count := 0
	for _, entry := range list {
		obj, ok := entry.(map[string]any)
		if ok && obj["marketplace"] == marketplace && obj["plugin"] == plugin {
			count++
		}
	}
	return count
}
