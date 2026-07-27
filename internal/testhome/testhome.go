// Package testhome sandboxes a test's home-derived state so nothing escapes
// into the developer's real home directory.
package testhome

import "testing"

// daemonkit resolves the home directory through the passwd database and ignores
// HOME; its own override constant lives in daemonkit/internal/realhome, which no
// consumer can import, so the name is spelled out here.
const daemonkitHomeEnv = "DAEMONKIT_HOME"

// Set points both HOME and daemonkit's home override at dir.
func Set(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv(daemonkitHomeEnv, dir)
}

// Isolate points both HOME and daemonkit's home override at one fresh temp dir.
func Isolate(t *testing.T) {
	t.Helper()
	Set(t, t.TempDir())
}
