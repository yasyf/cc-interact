package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/paths"
)

func testLauncherPaths() paths.Paths { return paths.Paths{App: ".cc-interact-launcher-test"} }

// exactLauncher is a launcher every field of which validate accepts, so a case
// states only what it breaks.
func exactLauncher(spec daemonkit.Daemon) Launcher {
	return Launcher{Daemon: spec, Paths: testLauncherPaths(), RuntimeBuild: "1.0.0"}
}

func testLauncherSpec(label string) daemonkit.Daemon {
	return Spec(daemonkit.Daemon{
		Label: daemonkit.Label(label),
		Trust: daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	})
}

func TestLauncherValidateRequiresSpecIdentity(t *testing.T) {
	exact := exactLauncher(testLauncherSpec("cci-launcher-test"))
	tests := []struct {
		name    string
		mutate  func(Launcher) Launcher
		wantErr bool
	}{
		{"exact", func(l Launcher) Launcher { return l }, false},
		{"missing schema", func(l Launcher) Launcher { l.Daemon.Schemas = nil; return l }, true},
		{"foreign schema", func(l Launcher) Launcher {
			l.Daemon.Schemas = []daemonkit.Schema{"legacy"}
			return l
		}, true},
		{"prior era beside it", func(l Launcher) Launcher {
			l.Daemon.Schemas = []daemonkit.Schema{WireBuild, "legacy"}
			return l
		}, true},
		{"unstated serving", func(l Launcher) Launcher {
			l.Daemon.Trust.Serving = daemonkit.Serving{}
			return l
		}, true},
		{"missing label", func(l Launcher) Launcher { l.Daemon.Label = ""; return l }, true},
		{"missing paths", func(l Launcher) Launcher { l.Paths = paths.Paths{}; return l }, true},
		{"missing runtime build", func(l Launcher) Launcher { l.RuntimeBuild = ""; return l }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.mutate(exact).validate(); (err != nil) != tt.wantErr {
				t.Fatalf("validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSpecStampsSchemaAndFrameDefault(t *testing.T) {
	spec := Spec(daemonkit.Daemon{Label: "cci-spec-test"})
	if len(spec.Schemas) != 1 || spec.Schemas[0] != WireBuild {
		t.Fatalf("Schemas = %v, want exactly [%q]", spec.Schemas, WireBuild)
	}
	if spec.MaxFrame != maxFrameBytes {
		t.Fatalf("MaxFrame = %d, want %d", spec.MaxFrame, maxFrameBytes)
	}
	if got := daemonkit.MaxDetail(spec.MaxFrame); got < maxPayloadBytes {
		t.Fatalf("MaxDetail(%d) = %d, want at least the %d-byte Write payload the gate must see", spec.MaxFrame, got, maxPayloadBytes)
	}
	if spec.Restart != daemonkit.RestartAlways {
		t.Fatalf("Restart = %v, want RestartAlways for an unstated policy", spec.Restart)
	}
	pinned := Spec(daemonkit.Daemon{Label: "cci-spec-test", MaxFrame: 256, Restart: daemonkit.RestartOnFailure})
	if pinned.MaxFrame != 256 {
		t.Fatalf("MaxFrame = %d, want the caller's 256", pinned.MaxFrame)
	}
	if pinned.Restart != daemonkit.RestartOnFailure {
		t.Fatalf("Restart = %v, want the caller's RestartOnFailure", pinned.Restart)
	}
}

func TestNewClientRefusesAForeignSchema(t *testing.T) {
	base := daemonkit.Daemon{
		Label: "cci-client-schema-test",
		Trust: daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	}
	tests := []struct {
		name    string
		schemas []daemonkit.Schema
	}{
		{"unstated", nil},
		{"foreign", []daemonkit.Schema{"legacy"}},
		{"prior era beside it", []daemonkit.Schema{WireBuild, "legacy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base
			d.Schemas = tt.schemas
			if _, err := NewClient(d); err == nil {
				t.Fatalf("NewClient accepted schemas %v", tt.schemas)
			}
		})
	}
}

// TestALauncherOrdersItselfAgainstAServersOwnReport joins the two halves of the
// ordering channel: what a daemon of one release publishes is what a launcher
// of an older one reads back and refuses. The wire hop between them —
// Ctx.Report into Health.Detail — is daemonkit's own and stays covered there,
// because Control refuses to pin its own process and so cannot read a daemon
// this test would have to serve in-process.
func TestALauncherOrdersItselfAgainstAServersOwnReport(t *testing.T) {
	detail := (&Server{runtimeBuild: "2.0.0"}).healthDetail()
	build, ok := reportedBuild(detail)
	if !ok || build != "2.0.0" {
		t.Fatalf("reportedBuild = %q, %v; want %q, true", build, ok, "2.0.0")
	}
	err := (Launcher{RuntimeBuild: "1.0.0"}).refuseRollback(daemonkit.Health{Detail: detail})
	if !errors.Is(err, ErrIncumbentNewer) {
		t.Fatalf("refuseRollback against a 2.0.0 incumbent = %v, want ErrIncumbentNewer", err)
	}
}

func TestRefuseRollbackOrdersOnlyOnAReadableRelease(t *testing.T) {
	detail := func(build string) []byte {
		b, err := json.Marshal(HealthDetail{RuntimeBuild: build})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name    string
		detail  []byte
		wantErr bool
	}{
		{"newer refuses", detail("2.0.0"), true},
		{"newer patch refuses", detail("1.0.1"), true},
		{"same proceeds", detail("1.0.0"), false},
		{"older proceeds", detail("0.9.0"), false},
		{"absent detail proceeds", nil, false},
		{"empty detail proceeds", []byte{}, false},
		{"undecodable detail proceeds", []byte("not json"), false},
		{"trailing value proceeds", append(detail("2.0.0"), " {}"...), false},
		{"unstated release proceeds", detail(""), false},
	}
	launcher := Launcher{RuntimeBuild: "1.0.0"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := launcher.refuseRollback(daemonkit.Health{Detail: tt.detail})
			if errors.Is(err, ErrIncumbentNewer) != tt.wantErr {
				t.Fatalf("refuseRollback() = %v, want ErrIncumbentNewer %v", err, tt.wantErr)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("refuseRollback() = %v, want nil", err)
			}
		})
	}
}

func TestClassifyAbsenceNamesAPre021Daemon(t *testing.T) {
	shortHome(t)
	launcher := exactLauncher(testLauncherSpec("cci-launcher-absence"))
	if err := launcher.classifyAbsence(); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("classifyAbsence with no legacy socket = %v, want ErrNoPeer", err)
	}
	writeLegacySocket(t, launcher.Paths)
	err := launcher.classifyAbsence()
	if !errors.Is(err, ErrLegacyDaemon) {
		t.Fatalf("classifyAbsence beside a legacy socket = %v, want ErrLegacyDaemon", err)
	}
	if !strings.Contains(err.Error(), launcher.Paths.SocketPath()) {
		t.Fatalf("refusal %q does not name the legacy socket", err)
	}
}

// TestEnsureCurrentIfRunningReportsALegacyDaemon pins the one thing this build
// can still learn about a pre-0.21 daemon: its socket exists, so absence on the
// label-derived endpoint is this era's absence and not the daemon's.
func TestEnsureCurrentIfRunningReportsALegacyDaemon(t *testing.T) {
	shortHome(t)
	launcher := exactLauncher(testLauncherSpec("cci-launcher-legacy"))
	if err := launcher.EnsureCurrentIfRunning(context.Background()); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("EnsureCurrentIfRunning with nothing running = %v, want ErrNoPeer", err)
	}
	writeLegacySocket(t, launcher.Paths)
	if err := launcher.EnsureCurrentIfRunning(context.Background()); !errors.Is(err, ErrLegacyDaemon) {
		t.Fatalf("EnsureCurrentIfRunning beside a legacy socket = %v, want ErrLegacyDaemon", err)
	}
}

func writeLegacySocket(t *testing.T, p paths.Paths) {
	t.Helper()
	if err := p.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.SocketPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
