package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestAuthHandler(t *testing.T) {
	const token = "s3cret-token"
	trustIP := func(want string) func(netip.Addr) bool {
		return func(a netip.Addr) bool { return a == netip.MustParseAddr(want) }
	}
	trustHost := func(want string) func(string) bool {
		return func(h string) bool { return h == want }
	}
	tests := []struct {
		name        string
		token       string
		remoteAddr  string
		authHeader  string
		queryToken  string
		origin      string
		fetchSite   string
		trustPeer   func(netip.Addr) bool
		trustOrigin func(string) bool
		wantStatus  int
		wantBody    string
	}{
		{"loopback v4 bypasses", token, "127.0.0.1:41000", "", "", "", "", nil, nil, http.StatusOK, "reached"},
		{"loopback v6 bypasses", token, "[::1]:41000", "", "", "", "", nil, nil, http.StatusOK, "reached"},
		{"header token accepted", token, "192.168.1.9:41000", "Bearer " + token, "", "", "", nil, nil, http.StatusOK, "reached"},
		{"query token accepted", token, "192.168.1.9:41000", "", token, "", "", nil, nil, http.StatusOK, "reached"},
		{"wrong token rejected", token, "192.168.1.9:41000", "Bearer nope", "", "", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"missing token rejected", token, "192.168.1.9:41000", "", "", "", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"empty token loopback bypasses", "", "127.0.0.1:41000", "", "", "", "", nil, nil, http.StatusOK, "reached"},
		{"empty token non-loopback rejected", "", "192.168.1.9:41000", "", "", "", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"loopback with loopback origin bypasses", token, "127.0.0.1:41000", "", "", "http://127.0.0.1:8123", "", nil, nil, http.StatusOK, "reached"},
		{"loopback with localhost origin bypasses", token, "127.0.0.1:41000", "", "", "http://localhost:8123", "", nil, nil, http.StatusOK, "reached"},
		{"loopback with foreign origin rejected", token, "127.0.0.1:41000", "", "", "https://evil.example", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"loopback with null origin rejected", token, "127.0.0.1:41000", "", "", "null", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"loopback with foreign origin and token accepted", token, "127.0.0.1:41000", "Bearer " + token, "", "https://evil.example", "", nil, nil, http.StatusOK, "reached"},
		{"empty token loopback with foreign origin rejected", "", "127.0.0.1:41000", "", "", "https://evil.example", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"trusted peer no origin bypasses", token, "100.64.0.7:41000", "", "", "", "", trustIP("100.64.0.7"), nil, http.StatusOK, "reached"},
		{"trusted peer trusted origin bypasses", token, "100.64.0.7:41000", "", "", "http://me.tailnet.example:8123", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusOK, "reached"},
		{"trusted peer trusted ip origin bypasses", token, "100.64.0.7:41000", "", "", "http://100.64.0.7:8123", "", trustIP("100.64.0.7"), trustHost("100.64.0.7"), http.StatusOK, "reached"},
		{"trusted peer foreign origin rejected", token, "100.64.0.7:41000", "", "", "https://evil.example", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusUnauthorized, "unauthorized\n"},
		{"trusted peer localhost origin rejected", token, "100.64.0.7:41000", "", "", "http://localhost:8123", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusUnauthorized, "unauthorized\n"},
		{"trusted peer loopback ip origin rejected", token, "100.64.0.7:41000", "", "", "http://127.0.0.1:8123", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusUnauthorized, "unauthorized\n"},
		{"trusted peer loopback v6 origin rejected", token, "100.64.0.7:41000", "", "", "http://[::1]:8123", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusUnauthorized, "unauthorized\n"},
		{"trusted peer localhost origin with token accepted", token, "100.64.0.7:41000", "Bearer " + token, "", "http://localhost:8123", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusOK, "reached"},
		{"trusted peer localhost origin trusted hook approves", token, "100.64.0.7:41000", "", "", "http://localhost:8123", "", trustIP("100.64.0.7"), trustHost("localhost"), http.StatusOK, "reached"},
		{"trusted peer foreign origin with token accepted", token, "100.64.0.7:41000", "Bearer " + token, "", "https://evil.example", "", trustIP("100.64.0.7"), trustHost("me.tailnet.example"), http.StatusOK, "reached"},
		{"empty token trusted peer bypasses", "", "100.64.0.7:41000", "", "", "", "", trustIP("100.64.0.7"), nil, http.StatusOK, "reached"},
		{"untrusted peer with hook rejected", "", "192.168.1.9:41000", "", "", "", "", trustIP("100.64.0.7"), nil, http.StatusUnauthorized, "unauthorized\n"},
		{"v4-in-v6 trusted peer unmapped", token, "[::ffff:100.64.0.7]:41000", "", "", "", "", trustIP("100.64.0.7"), nil, http.StatusOK, "reached"},
		{"loopback with trusted origin bypasses", token, "127.0.0.1:41000", "", "", "http://me.tailnet.example:8123", "", nil, trustHost("me.tailnet.example"), http.StatusOK, "reached"},
		{"trusted origin without peer trust rejected", "", "192.168.1.9:41000", "", "", "http://me.tailnet.example:8123", "", nil, trustHost("me.tailnet.example"), http.StatusUnauthorized, "unauthorized\n"},
		{"cross-site no origin loopback rejected", token, "127.0.0.1:41000", "", "", "", "cross-site", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"cross-site no origin trusted peer rejected", token, "100.64.0.7:41000", "", "", "", "cross-site", trustIP("100.64.0.7"), nil, http.StatusUnauthorized, "unauthorized\n"},
		{"cross-site mixed case no origin rejected", token, "127.0.0.1:41000", "", "", "", "Cross-Site", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"same-origin no origin loopback bypasses", token, "127.0.0.1:41000", "", "", "", "same-origin", nil, nil, http.StatusOK, "reached"},
		{"zoned v6 loopback rejected", "", "[::1%en0]:41000", "", "", "", "", nil, nil, http.StatusUnauthorized, "unauthorized\n"},
		{"zoned v6 loopback with token accepted", token, "[::1%en0]:41000", "Bearer " + token, "", "", "", nil, nil, http.StatusOK, "reached"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("reached"))
			})
			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			if tt.queryToken != "" {
				q := req.URL.Query()
				q.Set("token", tt.queryToken)
				req.URL.RawQuery = q.Encode()
			}
			rec := httptest.NewRecorder()
			authHandler(tt.token, tt.trustPeer, tt.trustOrigin, next).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestAuthHandlerStripsQueryToken(t *testing.T) {
	const token = "s3cret-token"
	var gotQuery url.Values
	var gotURI string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/events?token="+token+"&x=1", nil)
	req.RemoteAddr = "192.168.1.9:41000"
	rec := httptest.NewRecorder()
	authHandler(token, nil, nil, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotQuery.Has("token") {
		t.Fatalf("token survived into downstream query: %v", gotQuery)
	}
	if got := gotQuery.Get("x"); got != "1" {
		t.Fatalf("x = %q, want %q", got, "1")
	}
	if strings.Contains(gotURI, "token") {
		t.Fatalf("token survived into RequestURI: %q", gotURI)
	}
}

func TestPublicFallback(t *testing.T) {
	const token = "s3cret-token"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("events"))
	})
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sessions"))
	})
	public := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("spa"))
	})
	handler := publicFallback(mux, authHandler(token, nil, nil, mux), public)

	tests := []struct {
		name       string
		path       string
		authHeader string
		wantStatus int
		wantBody   string
	}{
		{"unmatched path is public without token", "/", "", http.StatusOK, "spa"},
		{"asset path is public without token", "/assets/app.js", "", http.StatusOK, "spa"},
		{"mounted route requires token", "/events", "", http.StatusUnauthorized, "unauthorized\n"},
		{"mounted route serves with token", "/api/sessions", "Bearer " + token, http.StatusOK, "sessions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.RemoteAddr = "192.168.1.9:41000"
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestValidateBindAuth(t *testing.T) {
	tests := []struct {
		name           string
		bindAddr       string
		token          string
		extraListeners bool
		trustedPeer    bool
		wantErr        bool
	}{
		{"empty addr empty token ok", "", "", false, false, false},
		{"loopback v4 empty token ok", "127.0.0.1", "", false, false, false},
		{"loopback v6 empty token ok", "::1", "", false, false, false},
		{"wildcard v4 empty token refused", "0.0.0.0", "", false, false, true},
		{"wildcard v6 empty token refused", "::", "", false, false, true},
		{"lan ip empty token refused", "192.168.1.9", "", false, false, true},
		{"wildcard v4 with token ok", "0.0.0.0", "s3cret", false, false, false},
		{"lan ip with token ok", "192.168.1.9", "s3cret", false, false, false},
		{"extra listeners empty token refused", "", "", true, false, true},
		{"extra listeners with token ok", "", "s3cret", true, false, false},
		{"extra listeners lan ip with token ok", "192.168.1.9", "s3cret", true, false, false},
		{"wildcard v4 no token trusted ok", "0.0.0.0", "", false, true, false},
		{"extra listeners no token trusted ok", "", "", true, true, false},
		{"lan ip no token trusted ok", "192.168.1.9", "", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBindAuth(bindHostOrDefault(tt.bindAddr), tt.token, tt.extraListeners, tt.trustedPeer)
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticatedBind) {
					t.Fatalf("err = %v, want ErrUnauthenticatedBind", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}
