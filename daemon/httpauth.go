package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// ErrUnauthenticatedBind is returned when the HTTP plane would bind a
// non-loopback address with no token and no TrustedPeer hook, which would
// serve every off-host request unauthenticated.
var ErrUnauthenticatedBind = errors.New("non-loopback HTTP bind requires a token")

// authHandler guards next with three acceptance paths. An unzoned loopback
// request passes without a token, as does a request whose peer IP the
// trustPeer hook approves (the address is Unmap()ed first, so a v4-in-v6
// ::ffff: peer counts as its v4 form; an unparseable RemoteAddr never
// bypasses, and a zoned peer reaches only the hook) — both only under an
// allowed Origin, per allowedOrigin, so a
// foreign page cannot CSRF the daemon through a browser on a local or trusted
// machine. Otherwise the request must present the token in an "Authorization:
// Bearer <token>" header or the ?token= query fallback (browser EventSource
// cannot set headers), compared in constant time; a ?token= is stripped before
// next runs so it never reaches a downstream redirect or access log. Anything
// else is 401 — with no token and no hooks configured (a loopback-only bind,
// per validateBindAuth) only the loopback bypass admits requests. The bypasses
// trust the immediate TCP peer, so fronting the daemon with a local reverse
// proxy — tailscale serve included — defeats token auth and peer trust alike:
// an unsupported deployment.
func authHandler(token string, trustPeer func(netip.Addr) bool, trustOrigin func(string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate := r.URL.Query().Get("token")
		stripTokenParam(r)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if addr, err := netip.ParseAddr(host); err == nil {
			addr = addr.Unmap()
			loopbackPeer := addr.IsLoopback() && addr.Zone() == ""
			trusted := loopbackPeer || (trustPeer != nil && trustPeer(addr))
			if trusted && allowedOrigin(r, trustOrigin, loopbackPeer) {
				next.ServeHTTP(w, r)
				return
			}
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

// allowedOrigin reports whether r's Origin header permits a no-token bypass
// (loopback or trusted peer): absent Origin with Sec-Fetch-Site not saying
// "cross-site" (cross-site GET navigations omit Origin per the Fetch spec;
// native clients send neither header and pass), localhost or a loopback host
// (the daemon's own SPA) only when the connection itself is loopback — a
// trusted peer's browser saying "localhost" means a page on the peer's
// machine, not this one — or a host the trusted hook approves — the daemon's
// own advertised names, never peers'. Anything else — a foreign site, an
// opaque "null" — must present the token.
func allowedOrigin(r *http.Request, trusted func(string) bool, loopbackPeer bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return !strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if loopbackPeer {
		if host == "localhost" {
			return true
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return trusted != nil && trusted(host)
}

// publicFallback routes requests the mux has a registered pattern for through
// authed, and everything else to public — the consumer's static catch-all,
// which must stay fetchable before a browser holds the token.
func publicFallback(mux *http.ServeMux, authed, public http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			authed.ServeHTTP(w, r)
			return
		}
		public.ServeHTTP(w, r)
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
// unauthenticated. A token or a TrustedPeer hook authenticates off-host
// requests, so either permits any bind. With neither, a non-loopback bind —
// where authHandler's loopback bypass never applies and there is nothing to
// check a request against — is refused, as is any extra listener, whose peers
// may be non-loopback for the same reason.
func validateBindAuth(bindHost, token string, extraListeners, trustedPeer bool) error {
	if token != "" || trustedPeer {
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
