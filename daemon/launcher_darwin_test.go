package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/launchd"
)

// TestStopRemovesTheAgentWithoutAPlacedProgram drives the interleaving every
// `stop` on a machine that never ensured this era hits: the agent installed,
// the label-leaf program never placed, no Ensure before it. Stop renders no
// agent and places nothing, so the executable it never needs must not gate the
// removal.
func TestStopRemovesTheAgentWithoutAPlacedProgram(t *testing.T) {
	shortHome(t)
	program, err := daemonkit.Stable()
	if err != nil {
		t.Fatalf("Stable: %v", err)
	}
	const label = "cci-launcher-stop-unplaced"
	launcher := exactLauncher(Spec(daemonkit.Daemon{
		Label:   label,
		Program: program,
		Trust:   daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	}))
	plist := writeMarkedAgent(t, label)
	if _, err := os.Stat(programPath(label)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("program leaf exists before any Ensure: err = %v", err)
	}
	if err := launcher.Stop(context.Background(), 30*time.Second); err != nil {
		t.Fatalf("Stop() = %v, want the LaunchAgent removed", err)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LaunchAgent %q survived Stop: err = %v", plist, err)
	}
}

func programPath(label string) string {
	return filepath.Join(os.Getenv("HOME"), ".daemonkit", "bin", label)
}

// writeMarkedAgent installs a plist carrying daemonkit's ownership marker, the
// only shape Stop's removal takes down.
func writeMarkedAgent(t *testing.T, label string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, label+".plist")
	body := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>` + label + `</string>
<key>EnvironmentVariables</key><dict><key>` + launchd.OwnerEnvKey + `</key><string>` + label + `</string></dict></dict></plist>
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
