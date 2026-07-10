package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ErrUnauthenticatedBind is returned when the HTTP plane would bind a
// non-loopback address with no token, which would serve every off-host request
// unauthenticated.
var ErrUnauthenticatedBind = errors.New("non-loopback HTTP bind requires a token")

// authHandler guards next with a bearer token. An empty token disables the
// check. A loopback request always passes — ParseIP unmaps ::ffff:127.0.0.1, so
// v4-in-v6 counts as loopback; a RemoteAddr that does not parse to an IP does
// not. Otherwise the request must present the token in an "Authorization: Bearer
// <token>" header or the ?token= query fallback (which browser EventSource needs,
// since it cannot set headers), compared in constant time; a ?token= is stripped
// from the URL before next runs, so it never survives into a downstream redirect
// or access log. Anything else is 401. The loopback bypass trusts the immediate
// TCP peer, so fronting the daemon with a local reverse proxy makes every proxied
// request appear loopback and defeats token auth — an unsupported deployment.
func authHandler(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate := r.URL.Query().Get("token")
		stripTokenParam(r)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			next.ServeHTTP(w, r)
			return
		}
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			candidate = strings.TrimPrefix(h, "Bearer ")
		}
		if tokensMatch(candidate, token) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// tokensMatch reports whether candidate equals token, comparing SHA-256 digests
// in constant time. Hashing to a fixed 32-byte width first means a length
// mismatch cannot be timed, which a raw ConstantTimeCompare of the strings would
// leak.
func tokensMatch(candidate, token string) bool {
	c := sha256.Sum256([]byte(candidate))
	t := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(c[:], t[:]) == 1
}

// stripTokenParam removes the ?token= query parameter from r so a token that
// authenticated the request never reaches a downstream redirect Location or
// access log. It is a no-op when the request carries no token param.
func stripTokenParam(r *http.Request) {
	q := r.URL.Query()
	if !q.Has("token") {
		return
	}
	q.Del("token")
	r.URL.RawQuery = q.Encode()
	r.RequestURI = r.URL.RequestURI()
}

// loopbackBind reports whether host is a loopback address the auth layer trusts
// to bypass the token check. A wildcard ("0.0.0.0", "::") or any other
// non-loopback address is not, and an unparseable host is treated as non-loopback
// so an ambiguous bind still requires a token.
func loopbackBind(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateBindAuth rejects a configuration that would expose the HTTP plane
// unauthenticated: a non-loopback bind with no token, where authHandler's
// loopback bypass never applies and there is nothing to check a request against.
func validateBindAuth(bindHost, token string) error {
	if token == "" && !loopbackBind(bindHost) {
		return fmt.Errorf("bind %q: %w", bindHost, ErrUnauthenticatedBind)
	}
	return nil
}
