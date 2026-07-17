// Package channelsetup approves Claude Code plugins as channels.
//
// Managed settings paths are supported on darwin and linux only. Windows is
// deliberately unsupported because cc-interact's Unix-socket IPC already
// excludes Windows consumers.
package channelsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const shellSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@%+=:,./-_"

// Plugin identifies a Claude Code channel plugin by marketplace and name.
type Plugin struct {
	// Marketplace is the Claude Code marketplace the plugin installs from.
	Marketplace string
	// Name is the plugin's name within the marketplace.
	Name string
}

// ChannelID returns the plugin identifier Claude Code accepts for --channels.
func (p Plugin) ChannelID() string { return "plugin:" + p.Name + "@" + p.Marketplace }

// Source returns the source attribute Claude Code renders for the plugin's MCP server.
func (p Plugin) Source(server string) string { return "plugin:" + p.Name + ":" + server }

// ManagedSettingsPath returns Claude Code's machine-wide managed settings path.
func ManagedSettingsPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json", nil
	case "linux":
		return "/etc/claude-code/managed-settings.json", nil
	default:
		return "", fmt.Errorf("managed settings are unsupported on %s", runtime.GOOS)
	}
}

// ManagedHasEntry reports whether channel delivery is enabled and the plugin is allowlisted.
func (p Plugin) ManagedHasEntry(existing []byte) (bool, error) {
	m, err := decodeObject(existing)
	if err != nil {
		return false, err
	}
	list, err := allowlist(m)
	if err != nil {
		return false, err
	}
	return m["channelsEnabled"] == true && p.allowlistHasEntry(list), nil
}

// MergeManaged enables channels and adds the plugin to the managed allowlist.
func (p Plugin) MergeManaged(existing []byte) ([]byte, error) {
	m, err := decodeObject(existing)
	if err != nil {
		return nil, err
	}
	list, err := allowlist(m)
	if err != nil {
		return nil, err
	}
	m["channelsEnabled"] = true
	if !p.allowlistHasEntry(list) {
		list = append(list, map[string]any{"marketplace": p.Marketplace, "plugin": p.Name})
	}
	m["allowedChannelPlugins"] = list
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode managed settings: %w", err)
	}
	return append(out, '\n'), nil
}

// ApplyManagedViaAdmin writes merged settings through a macOS admin prompt.
func (p Plugin) ApplyManagedViaAdmin(merged []byte) (retErr error) {
	dest, err := ManagedSettingsPath()
	if err != nil {
		return fmt.Errorf("resolve managed settings path: %w", err)
	}
	tmp, err := os.CreateTemp("", p.Name+"-managed-*.json")
	if err != nil {
		return fmt.Errorf("create temp settings: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			if err := os.Remove(tmpPath); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("remove temp settings: %w", err))
			}
		}
	}()
	if _, err := tmp.Write(merged); err != nil {
		return errors.Join(fmt.Errorf("write temp settings: %w", err), closeTempSettings(tmp))
	}
	if err := closeTempSettings(tmp); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		removeTemp = false
		return fmt.Errorf("automatic managed-settings write is macOS-only; run: sudo install -d -m 755 %s && sudo install -m 644 %s %s", shellQuote(filepath.Dir(dest)), shellQuote(tmpPath), shellQuote(dest))
	}
	if out, err := exec.Command("osascript", "-e", adminScript(dest, tmpPath)).CombinedOutput(); err != nil { //nolint:gosec // G204: adminScript shell-quotes + AppleScript-escapes every path (TestAdminScriptQuoting)
		return fmt.Errorf("admin write of %s (%s): %w", dest, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Offer reports whether a consumer should offer channel approval and why.
func Offer(p Plugin, markerPath, managedPath string) (offer bool, reason string, err error) {
	if _, err := os.Stat(markerPath); err == nil {
		return false, "already offered", nil
	} else if !os.IsNotExist(err) {
		return false, "", fmt.Errorf("stat channel marker: %w", err)
	}
	managed, err := readFileOrEmpty(managedPath)
	if err != nil {
		return false, "", err
	}
	approved, err := p.ManagedHasEntry(managed)
	if err != nil {
		return false, "", err
	}
	if approved {
		return false, "already approved", nil
	}
	return true, "channel not yet approved", nil
}

func adminScript(dest, tmpPath string) string {
	shellCmd := fmt.Sprintf("mkdir -p %s && cp %s %s && chmod 644 %s",
		shellQuote(filepath.Dir(dest)), shellQuote(tmpPath), shellQuote(dest), shellQuote(dest))
	return `do shell script "` + appleScriptQuote(shellCmd) + `" with administrator privileges`
}

func appleScriptQuote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !strings.ContainsRune(shellSafe, r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

func closeTempSettings(tmp *os.File) error {
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp settings: %w", err)
	}
	return nil
}

func decodeObject(b []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse managed settings: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("managed settings: not a JSON object")
	}
	return m, nil
}

func allowlist(m map[string]any) ([]any, error) {
	raw, ok := m["allowedChannelPlugins"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("managed settings: allowedChannelPlugins is not an array")
	}
	return list, nil
}

func (p Plugin) allowlistHasEntry(list []any) bool {
	for _, entry := range list {
		obj, ok := entry.(map[string]any)
		if ok && obj["marketplace"] == p.Marketplace && obj["plugin"] == p.Name {
			return true
		}
	}
	return false
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
