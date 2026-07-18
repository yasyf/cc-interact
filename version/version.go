// Package version compares build version strings. The daemon's
// same-or-newer-wins socket eviction is the one piece of cc-interact that must
// order two builds; the algorithm is pure and domain-agnostic. The build
// metadata itself (the binary's own version string) is injected by the consumer
// via Config.Version, never by this package.
package version

import dkversion "github.com/yasyf/daemonkit/version"

// Newer reports whether build a is strictly newer than b, delegating to
// daemonkit's Release/Dev taxonomy so the socket eviction and the client version
// gate share one polarity: every dev build outranks every release, ties never win.
func Newer(a, b string) bool { return dkversion.Newer(a, b) }
