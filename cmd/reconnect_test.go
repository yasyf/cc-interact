package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestChannelDownOnlyOnceABeatSinceTheWindowStartedGoesStale(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "1.beat")
	now := time.Now()
	windowStarted := now.Add(-time.Hour).UnixMilli()
	if channelDown(beat, windowStarted, now) {
		t.Fatal("a window whose channel never beat must not read down: a reconnect cannot start a server that never ran")
	}
	if err := touch(beat); err != nil {
		t.Fatal(err)
	}
	if channelDown(beat, windowStarted, now) {
		t.Fatal("a fresh beat read down")
	}
	stale := now.Add(-2 * channelStale)
	if err := os.Chtimes(beat, stale, stale); err != nil {
		t.Fatal(err)
	}
	if !channelDown(beat, windowStarted, now) {
		t.Fatal("a stale beat read up")
	}
	if channelDown(beat, now.Add(-channelStale).UnixMilli(), now) {
		t.Fatal("a beat from before the window started, left by a recycled pid, read down")
	}
}

func TestClaimReconnectStartsOnePerInterval(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "windows", "1.reconnect")
	now := time.Now()
	windowStarted := now.Add(-time.Hour).UnixMilli()
	if !claimReconnect(stamp, windowStarted, now) {
		t.Fatal("the first reconnect was refused")
	}
	if claimReconnect(stamp, windowStarted, now) {
		t.Fatal("a second reconnect within the interval was claimed")
	}
	earlier := now.Add(-reconnectEvery - time.Second)
	if err := os.Chtimes(stamp, earlier, earlier); err != nil {
		t.Fatal(err)
	}
	if !claimReconnect(stamp, windowStarted, now) {
		t.Fatal("a reconnect after the interval was refused")
	}
	if !claimReconnect(stamp, time.Now().Add(time.Minute).UnixMilli(), time.Now()) {
		t.Fatal("a recycled pid inherited its predecessor's reconnect")
	}
}

type fakeOrca struct {
	busy  int
	draft string
	sent  []string
}

func (o *fakeOrca) run(_ context.Context, args ...string) (string, error) {
	switch args[1] {
	case "wait":
		if o.busy > 0 {
			o.busy--
			return "", errors.New("timed out")
		}
		return "", nil
	case "read":
		return `{"result":{"terminal":{"source":"screen","draft":` + o.draft + `}}}`, nil
	case "send":
		o.sent = append(o.sent, strings.Join(args, " "))
		return "", nil
	}
	return "", errors.New("unexpected orca call " + strings.Join(args, " "))
}

func TestTypeReconnectTypesAtAnIdlePromptWithAnEmptyDraft(t *testing.T) {
	const command = "/mcp reconnect plugin:demo:channel"
	down := func() bool { return true }
	pauses := 0
	pause := func() { pauses++ }

	o := &fakeOrca{busy: 2, draft: `""`}
	if err := typeReconnect(context.Background(), o.run, "term_1", command, down, pause); err != nil {
		t.Fatal(err)
	}
	if want := []string{"terminal send --terminal term_1 --text " + command + " --enter"}; !slices.Equal(o.sent, want) || pauses != 2 {
		t.Fatalf("sent %q after %d pauses, want %q after the two busy waits", o.sent, pauses, want)
	}

	o = &fakeOrca{draft: `"half a thought"`}
	pauses = 0
	if err := typeReconnect(context.Background(), o.run, "term_1", command, down, pause); err != nil {
		t.Fatal(err)
	}
	if len(o.sent) != 0 || pauses != reconnectTries {
		t.Fatalf("typed %q over a draft; want nothing typed after %d tries", o.sent, reconnectTries)
	}

	o = &fakeOrca{draft: `""`}
	if err := typeReconnect(context.Background(), o.run, "term_1", command, func() bool { return false }, pause); err != nil {
		t.Fatal(err)
	}
	if len(o.sent) != 0 {
		t.Fatalf("typed %q for a channel that came back", o.sent)
	}
}

func TestNewerInstallPicksTheNewestUnorphanedReleaseNewerThanThisOne(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	root := filepath.Join(config, "plugins", "cache", "demo", "demo")
	install := func(dir, prints string, orphaned bool) {
		t.Helper()
		bin := filepath.Join(root, dir, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "demo"), []byte("#!/bin/sh\necho demo version "+prints+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if orphaned {
			if err := os.WriteFile(filepath.Join(root, dir, ".orphaned_at"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	install("1.2.0", "1.2.0", false)
	install("1.10.0", "1.10.0", false)
	install("1.11.0", "1.11.0", true)
	d := Deps{Version: "v1.9.0"}
	d.Plugin.Marketplace, d.Plugin.Name = "demo", "demo"

	if path, ok := newerInstall(context.Background(), d, "demo"); !ok || path != filepath.Join(root, "1.10.0", "bin", "demo") {
		t.Fatalf("newerInstall = %q, %v; want 1.10.0, the newest release Claude Code has not orphaned", path, ok)
	}
	d.Version = "1.10.0"
	if path, ok := newerInstall(context.Background(), d, "demo"); ok {
		t.Fatalf("newerInstall = %q for the release already running", path)
	}
	install("1.12.0", "1.10.0", false)
	if path, ok := newerInstall(context.Background(), d, "demo"); ok {
		t.Fatalf("newerInstall = %q for an install whose binary runs the release already running", path)
	}
	d.Version = "dev"
	if path, ok := newerInstall(context.Background(), d, "demo"); ok {
		t.Fatalf("newerInstall = %q for a dev build", path)
	}
}
