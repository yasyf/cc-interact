package version

import "testing"

// TestNewerPolarity locks the same-or-newer-wins polarity now sourced from
// daemonkit: newer releases win, ties never win, every dev build (plain, empty,
// or the 9999 nanosecond sentinel) outranks every release, and a git-describe
// suffix ties its base triple.
func TestNewerPolarity(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"newer release beats older", "v0.11.0", "v0.10.0", true},
		{"older release never beats newer", "v0.10.0", "v0.11.0", false},
		{"release tie is not newer", "v0.8.0", "v0.8.0", false},
		{"git-describe suffix ties the base triple", "v0.8.0-1-gabc123", "v0.8.0", false},
		{"dev beats every release", "dev", "v9.9.9", true},
		{"release never beats dev", "v9.9.9", "dev", false},
		{"empty is a dev build and beats a release", "", "v1.2.3", true},
		{"dev sentinel beats a release", "9999.100.0-dev", "v9.9.9", true},
		// Plain-dev versus sentinel ordering is daemonkit's and deliberate.
		{"dev sentinel beats plain dev", "9999.100.0-dev", "dev", true},
		{"plain dev loses to dev sentinel", "dev", "9999.100.0-dev", false},
		{"newer dev sentinel beats older", "9999.200.0-dev", "9999.100.0-dev", true},
		{"dev tie is not newer", "dev", "dev", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Newer(tc.a, tc.b); got != tc.want {
				t.Fatalf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
