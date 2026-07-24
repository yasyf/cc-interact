package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"
)

// UnsupportedSchemaPolicy selects how Open reacts to an existing database whose
// exact v1 schema it does not recognize. The zero value fails closed.
type UnsupportedSchemaPolicy uint8

const (
	// FailUnsupportedSchema fails Open with the verification error and leaves the
	// database untouched on disk. It is the zero-value default.
	FailUnsupportedSchema UnsupportedSchemaPolicy = iota
	// ArchiveUnsupportedSchema renames the offending database and its WAL sidecars
	// aside, opens a fresh store at the same path, and logs one warning. No data is
	// read or migrated — the drifted store is preserved only for forensics.
	ArchiveUnsupportedSchema
)

// Option configures Open.
type Option func(*openConfig)

type openConfig struct {
	unsupportedSchema UnsupportedSchemaPolicy
}

// WithUnsupportedSchema selects Open's reaction to an existing database that
// fails exact v1 verification. Without it, Open fails closed, leaving the store
// on disk (the safe default). Pass ArchiveUnsupportedSchema to rename the wedged
// store aside and start fresh, ending the crash-loop a drifted store would
// otherwise cause on every activation.
func WithUnsupportedSchema(policy UnsupportedSchemaPolicy) Option {
	return func(c *openConfig) { c.unsupportedSchema = policy }
}

// sqliteSidecars are the WAL-mode companion files sqlite keeps beside the main
// database; they move with it so a fresh store never inherits a stale WAL.
var sqliteSidecars = []string{"-wal", "-shm"}

// archiveUnsupportedStore renames an unrecognized-schema database aside as
// "<path>.<fingerprint>.<timestamp>.bak", carrying any -wal/-shm sidecars to the
// matching "<backup>-wal"/"<backup>-shm" names, and returns the backup path. The
// fingerprint is a short content digest so two distinct wedged stores never
// collide. No data is read or migrated.
func archiveUnsupportedStore(path string) (string, error) {
	fingerprint, err := fileFingerprint(path)
	if err != nil {
		return "", err
	}
	backup := fmt.Sprintf("%s.%s.%s.bak", path, fingerprint, time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.Rename(path, backup); err != nil {
		return "", fmt.Errorf("store: archive unsupported store: %w", err)
	}
	for _, suffix := range sqliteSidecars {
		if err := os.Rename(path+suffix, backup+suffix); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("store: archive unsupported store %s sidecar: %w", suffix, err)
		}
	}
	return backup, nil
}

func fileFingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("store: fingerprint unsupported store: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("store: fingerprint unsupported store: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil))[:12], nil
}
