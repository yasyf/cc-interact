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
		wantStatus int
		wantBody   string
	}{
		{"loopback v4 bypasses", token, "127.0.0.1:41000", "", "", http.StatusOK, "reached"},
		{"loopback v6 bypasses", token, "[::1]:41000", "", "", http.StatusOK, "reached"},
		{"header token accepted", token, "192.168.1.9:41000", "Bearer " + token, "", http.StatusOK, "reached"},
		{"query token accepted", token, "192.168.1.9:41000", "", token, http.StatusOK, "reached"},
		{"wrong token rejected", token, "192.168.1.9:41000", "Bearer nope", "", http.StatusUnauthorized, "unauthorized\n"},
		{"missing token rejected", token, "192.168.1.9:41000", "", "", http.StatusUnauthorized, "unauthorized\n"},
		{"empty token passes through", "", "192.168.1.9:41000", "", "", http.StatusOK, "reached"},
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
		name     string
		bindAddr string
		token    string
		wantErr  bool
	}{
		{"empty addr empty token ok", "", "", false},
		{"loopback v4 empty token ok", "127.0.0.1", "", false},
		{"loopback v6 empty token ok", "::1", "", false},
		{"wildcard v4 empty token refused", "0.0.0.0", "", true},
		{"wildcard v6 empty token refused", "::", "", true},
		{"lan ip empty token refused", "192.168.1.9", "", true},
		{"wildcard v4 with token ok", "0.0.0.0", "s3cret", false},
		{"lan ip with token ok", "192.168.1.9", "s3cret", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBindAuth(bindHostOrDefault(tt.bindAddr), tt.token)
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
