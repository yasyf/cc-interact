// Package version compares build version strings. The daemon's
// same-or-newer-wins socket eviction is the one piece of cc-interact that must
// order two builds; the algorithm is pure and domain-agnostic. The build
// metadata itself (the binary's own version string) is injected by the consumer
// via Config.Version, never by this package.
package version

import (
	"regexp"
	"strconv"
	"strings"
)

var releaseTriple = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// Newer reports whether a is a strictly newer build than b. A leading v?X.Y.Z
// triple is compared numerically; any suffix is ignored, so "v0.8.0-1-gHASH"
// ties with "v0.8.0", and ties are never newer. A string with no release
// triple is a dev build and ranks newest: dev beats every release and ties
// with dev. That polarity is deliberate — a dev daemon is never evicted, and a
// dev binary always takes over a release daemon — preserving the dev-daemon
// workflow.
func Newer(a, b string) bool {
	ta, aRelease := parseTriple(a)
	tb, bRelease := parseTriple(b)
	if !aRelease {
		return bRelease
	}
	if !bRelease {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return ta[i] > tb[i]
		}
	}
	return false
}

func parseTriple(s string) ([3]int, bool) {
	m := releaseTriple.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	var t [3]int
	for i := range t {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		t[i] = n
	}
	return t, true
}
