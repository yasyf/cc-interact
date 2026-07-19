package transcript

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fakeTranscriptCLI = `#!/bin/sh
if [ "$#" -ne 4 ] || [ "$1" != "watch" ] || [ "$2" != "--root" ] || [ "$3" != "$CC_TRANSCRIPT_FAKE_ROOT" ] || [ "$4" != "--json" ]; then
	exit 90
fi
case "$CC_TRANSCRIPT_FAKE_MODE" in
	output)
		printf '%s\n' "$CC_TRANSCRIPT_FAKE_OUTPUT"
		;;
	garbage)
		printf '%s\n' '{not json'
		;;
	nonzero)
		printf '%s\n' 'fake failure' >&2
		exit 17
		;;
	wait)
		: > "$CC_TRANSCRIPT_FAKE_STARTED"
		exec sleep 60
		;;
	*)
		exit 99
		;;
esac
`

func TestSubagentsDir(t *testing.T) {
	tests := []struct {
		name              string
		sessionTranscript string
		want              string
	}{
		{
			name:              "session transcript",
			sessionTranscript: filepath.Join("projects", "slug", "39bd1158.jsonl"),
			want:              filepath.Join("projects", "slug", "39bd1158", "subagents"),
		},
		{
			name:              "absolute session transcript",
			sessionTranscript: filepath.Join(string(filepath.Separator), "home", "me", ".claude", "projects", "slug", "session.jsonl"),
			want:              filepath.Join(string(filepath.Separator), "home", "me", ".claude", "projects", "slug", "session", "subagents"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubagentsDir(tt.sessionTranscript); got != tt.want {
				t.Fatalf("SubagentsDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTailEntries(t *testing.T) {
	user := "user"
	tests := []struct {
		name string
		line string
		want Entry
	}{
		{
			name: "derives agent id",
			line: `{"path":"/sessions/subagents/agent-ac381a165f588a8e7.jsonl","session_id":"39bd1158","is_sidechain":true,"uuid":"c0033052","kind":"user","role":"user","preview":"Call the tool"}`,
			want: Entry{
				Path:        "/sessions/subagents/agent-ac381a165f588a8e7.jsonl",
				SessionID:   "39bd1158",
				AgentID:     "ac381a165f588a8e7",
				IsSidechain: true,
				UUID:        "c0033052",
				Kind:        "user",
				Role:        &user,
				Preview:     "Call the tool",
			},
		},
		{
			name: "non-matching basename",
			line: `{"path":"/sessions/subagents/transcript.jsonl","session_id":"39bd1158","is_sidechain":true,"uuid":"d0044163","kind":"attachment","role":null,"preview":"Attached file"}`,
			want: Entry{
				Path:        "/sessions/subagents/transcript.jsonl",
				SessionID:   "39bd1158",
				IsSidechain: true,
				UUID:        "d0044163",
				Kind:        "attachment",
				Preview:     "Attached file",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			installFakeTranscript(t, dir, "output", tt.line)

			var got []Entry
			if err := Tail(context.Background(), dir, func(entry Entry) {
				got = append(got, entry)
			}); err != nil {
				t.Fatalf("Tail: %v", err)
			}
			if want := []Entry{tt.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("entries = %#v, want %#v", got, want)
			}
		})
	}
}

func TestTailWaitsForDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "session", "subagents")
	line := `{"path":"/sessions/subagents/agent-late.jsonl","session_id":"session","is_sidechain":true,"uuid":"uuid","kind":"assistant","role":"assistant","preview":"ready"}`
	installFakeTranscript(t, dir, "output", line)

	errCh := make(chan error, 1)
	entries := make(chan Entry, 1)
	go func() {
		errCh <- Tail(context.Background(), dir, func(entry Entry) {
			entries <- entry
		})
	}()

	time.Sleep(100 * time.Millisecond)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Tail did not observe the directory")
	}
	select {
	case entry := <-entries:
		if entry.AgentID != "late" {
			t.Fatalf("AgentID = %q, want %q", entry.AgentID, "late")
		}
	default:
		t.Fatal("Tail returned without delivering an entry")
	}
}

func TestTailContextCancel(t *testing.T) {
	dir := t.TempDir()
	installFakeTranscript(t, dir, "wait", "")
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("CC_TRANSCRIPT_FAKE_STARTED", started)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Tail(ctx, dir, func(Entry) {})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake cc-transcript did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Tail error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not return promptly after cancellation")
	}
}

func TestTailErrors(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{name: "malformed output", mode: "garbage", wantErr: "decode cc-transcript entry"},
		{name: "nonzero exit", mode: "nonzero", wantErr: "cc-transcript watch: exit status 17: fake failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			installFakeTranscript(t, dir, tt.mode, "")
			err := Tail(context.Background(), dir, func(Entry) {})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Tail error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestTailMissingCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	err := Tail(context.Background(), dir, func(Entry) {})
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Tail error = %v, want exec.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "start cc-transcript watch") {
		t.Fatalf("Tail error = %v, want start context", err)
	}
}

func installFakeTranscript(t *testing.T, root, mode, output string) {
	t.Helper()
	binDir := t.TempDir()
	path := filepath.Join(binDir, "cc-transcript")
	if err := os.WriteFile(path, []byte(fakeTranscriptCLI), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CC_TRANSCRIPT_FAKE_ROOT", root)
	t.Setenv("CC_TRANSCRIPT_FAKE_MODE", mode)
	t.Setenv("CC_TRANSCRIPT_FAKE_OUTPUT", output)
}

func TestAgentID(t *testing.T) {
	for _, tt := range []struct {
		name, path, want string
	}{
		{"plain", "/x/subagents/agent-a16ce1d0b96bc687d.jsonl", "a16ce1d0b96bc687d"},
		{"empty id", "/x/subagents/agent-.jsonl", ""},
		{"no prefix", "/x/subagents/main.jsonl", ""},
		{"no suffix", "/x/subagents/agent-a16ce1d0b96bc687d.meta.json", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentID(tt.path); got != tt.want {
				t.Fatalf("agentID(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
