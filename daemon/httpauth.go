package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ErrUnauthenticatedBind is returned when the HTTP plane would bind a
// non-loopback address with no token, which would serve every off-host request
// unauthenticated.
var ErrUnauthenticatedBind = errors.New("non-loopback HTTP bind requires a token")

// authHandler guards next with a bearer token. A loopback request passes
// without one (ParseIP unmaps ::ffff:127.0.0.1, so v4-in-v6 counts; an
// unparseable RemoteAddr does not) — but only under a loopback Origin, per
// loopbackOrigin, so a foreign page cannot CSRF the daemon through a local
// browser. Otherwise the request must present the token in an "Authorization:
// Bearer <token>" header or the ?token= query fallback (browser EventSource
// cannot set headers), compared in constant time; a ?token= is stripped before
// next runs so it never reaches a downstream redirect or access log. Anything
// else is 401 — with no token configured (a loopback-only bind, per
// validateBindAuth) only the loopback bypass admits requests. The bypass trusts
// the immediate TCP peer, so fronting the daemon with a local reverse proxy
// defeats token auth — an unsupported deployment.
func authHandler(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate := r.URL.Query().Get("token")
		stripTokenParam(r)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() && loopbackOrigin(r) {
			next.ServeHTTP(w, r)
			return
		}
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			candidate = strings.TrimPrefix(h, "Bearer ")
		}
		if token != "" && tokensMatch(candidate, token) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// loopbackOrigin reports whether r's Origin header permits the loopback-peer
// bypass: absent (a native client — browsers always send Origin on cross-origin
// requests and on every POST) or naming a loopback host (the daemon's own SPA).
// Anything else — a foreign site, an opaque "null" — must present the token.
func loopbackOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
// unauthenticated: with no token, a non-loopback bind — where authHandler's
// loopback bypass never applies and there is nothing to check a request against
// — and any extra listener, whose peers may be non-loopback for the same reason.
func validateBindAuth(bindHost, token string, extraListeners bool) error {
	if token != "" {
		return nil
	}
	if !loopbackBind(bindHost) {
		return fmt.Errorf("bind %q: %w", bindHost, ErrUnauthenticatedBind)
	}
	if extraListeners {
		return fmt.Errorf("extra HTTP listeners: %w", ErrUnauthenticatedBind)
	}
	return nil
}
