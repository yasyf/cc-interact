package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthHandler(t *testing.T) {
	const token = "s3cret-token"
	tests := []struct {
		name       string
		token      string
		remoteAddr string
		authHeader string
		queryToken string
		origin     string
		wantStatus int
		wantBody   string
	}{
		{"loopback v4 bypasses", token, "127.0.0.1:41000", "", "", "", http.StatusOK, "reached"},
		{"loopback v6 bypasses", token, "[::1]:41000", "", "", "", http.StatusOK, "reached"},
		{"header token accepted", token, "192.168.1.9:41000", "Bearer " + token, "", "", http.StatusOK, "reached"},
		{"query token accepted", token, "192.168.1.9:41000", "", token, "", http.StatusOK, "reached"},
		{"wrong token rejected", token, "192.168.1.9:41000", "Bearer nope", "", "", http.StatusUnauthorized, "unauthorized\n"},
		{"missing token rejected", token, "192.168.1.9:41000", "", "", "", http.StatusUnauthorized, "unauthorized\n"},
		{"empty token loopback bypasses", "", "127.0.0.1:41000", "", "", "", http.StatusOK, "reached"},
		{"empty token non-loopback rejected", "", "192.168.1.9:41000", "", "", "", http.StatusUnauthorized, "unauthorized\n"},
		{"loopback with loopback origin bypasses", token, "127.0.0.1:41000", "", "", "http://127.0.0.1:8123", http.StatusOK, "reached"},
		{"loopback with localhost origin bypasses", token, "127.0.0.1:41000", "", "", "http://localhost:8123", http.StatusOK, "reached"},
		{"loopback with foreign origin rejected", token, "127.0.0.1:41000", "", "", "https://evil.example", http.StatusUnauthorized, "unauthorized\n"},
		{"loopback with null origin rejected", token, "127.0.0.1:41000", "", "", "null", http.StatusUnauthorized, "unauthorized\n"},
		{"loopback with foreign origin and token accepted", token, "127.0.0.1:41000", "Bearer " + token, "", "https://evil.example", http.StatusOK, "reached"},
		{"empty token loopback with foreign origin rejected", "", "127.0.0.1:41000", "", "", "https://evil.example", http.StatusUnauthorized, "unauthorized\n"},
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
			if tt.queryToken != "" {
				q := req.URL.Query()
				q.Set("token", tt.queryToken)
				req.URL.RawQuery = q.Encode()
			}
			rec := httptest.NewRecorder()
			authHandler(tt.token, next).ServeHTTP(rec, req)
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
	authHandler(token, next).ServeHTTP(rec, req)

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

func TestValidateBindAuth(t *testing.T) {
	tests := []struct {
		name           string
		bindAddr       string
		token          string
		extraListeners bool
		wantErr        bool
	}{
		{"empty addr empty token ok", "", "", false, false},
		{"loopback v4 empty token ok", "127.0.0.1", "", false, false},
		{"loopback v6 empty token ok", "::1", "", false, false},
		{"wildcard v4 empty token refused", "0.0.0.0", "", false, true},
		{"wildcard v6 empty token refused", "::", "", false, true},
		{"lan ip empty token refused", "192.168.1.9", "", false, true},
		{"wildcard v4 with token ok", "0.0.0.0", "s3cret", false, false},
		{"lan ip with token ok", "192.168.1.9", "s3cret", false, false},
		{"extra listeners empty token refused", "", "", true, true},
		{"extra listeners with token ok", "", "s3cret", true, false},
		{"extra listeners lan ip with token ok", "192.168.1.9", "s3cret", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBindAuth(bindHostOrDefault(tt.bindAddr), tt.token, tt.extraListeners)
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
