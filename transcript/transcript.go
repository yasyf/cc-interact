// Package transcript maps Claude Code transcript paths.
package transcript

import (
	"path/filepath"
	"strings"
)

// SubagentsDir maps a session's main transcript path to its subagents
// transcript directory.
func SubagentsDir(sessionTranscript string) string {
	return filepath.Join(strings.TrimSuffix(sessionTranscript, ".jsonl"), "subagents")
}
