// Package transcript tails Claude Code subagent transcript events by running
// cc-transcript. It is an optional layer of cc-interact; cc-transcript owns the
// transcript format, and this package decodes only its NDJSON output.
package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const dirPollInterval = time.Second

// Entry is one subagent transcript event emitted by cc-transcript.
type Entry struct {
	Path        string  `json:"path"`
	SessionID   string  `json:"session_id"`
	AgentID     string  `json:"-"`
	IsSidechain bool    `json:"is_sidechain"`
	UUID        string  `json:"uuid"`
	Kind        string  `json:"kind"`
	Role        *string `json:"role"`
	Preview     string  `json:"preview"`
}

// SubagentsDir maps a session's main transcript path to its subagents
// transcript directory.
func SubagentsDir(sessionTranscript string) string {
	return filepath.Join(strings.TrimSuffix(sessionTranscript, ".jsonl"), "subagents")
}

// Tail watches dir for new subagent transcript entries and calls fn for each
// one. It waits for the lazily created directory and returns ctx.Err() when the
// context is cancelled.
func Tail(ctx context.Context, dir string, fn func(Entry)) error {
	if err := waitForDir(ctx, dir); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "cc-transcript", "watch", "--root", dir, "--json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open cc-transcript stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("start cc-transcript watch: %w", err)
	}

	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("decode cc-transcript entry: %w", err)
		}
		entry.AgentID = agentID(entry.Path)
		fn(entry)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan cc-transcript output: %w", err)
	}

	err = cmd.Wait()
	waited = true
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("cc-transcript watch: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func waitForDir(ctx context.Context, dir string) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := os.Stat(dir); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat transcript directory %s: %w", dir, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dirPollInterval):
		}
	}
}

func agentID(path string) string {
	name, ok := strings.CutPrefix(filepath.Base(path), "agent-")
	if !ok {
		return ""
	}
	id, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return ""
	}
	return id
}
