// Package statepath owns cc-interact's exact v1 derived-state namespace.
package statepath

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/daemonkit/paths"
)

const namespace = "cc-interact-v1"

// Dir returns the exact v1 derived-state directory.
func Dir(p paths.Paths) string { return filepath.Join(p.StateDir(), namespace) }

// DB returns the exact v1 SQLite store path.
func DB(p paths.Paths) string { return filepath.Join(Dir(p), "state.db") }

// SubjectDir returns one subject's exact v1 cursor directory.
func SubjectDir(p paths.Paths, subjectID string) string {
	return filepath.Join(Dir(p), "subjects", subjectID)
}

// Cursor returns one exact v1 consumer cursor path.
func Cursor(p paths.Paths, subjectID, consumer string) string {
	return filepath.Join(SubjectDir(p, subjectID), consumer+".cursor")
}

// EnsureDir creates the exact v1 state directory.
func EnsureDir(p paths.Paths) error {
	if err := os.MkdirAll(Dir(p), 0o700); err != nil {
		return fmt.Errorf("create v1 state directory: %w", err)
	}
	return nil
}

// EnsureSubjectDir creates one subject's exact v1 cursor directory.
func EnsureSubjectDir(p paths.Paths, subjectID string) error {
	if err := os.MkdirAll(SubjectDir(p, subjectID), 0o700); err != nil {
		return fmt.Errorf("create v1 subject directory: %w", err)
	}
	return nil
}
