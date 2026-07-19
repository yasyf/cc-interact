package sse

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticHandler(t *testing.T) {
	const indexHTML = "<!doctype html><title>app</title>"
	const pngBody = "\x89PNG\r\n\x1a\n"
	dist := fstest.MapFS{
		"index.html":         {Data: []byte(indexHTML)},
		"assets/present.png": {Data: []byte(pngBody)},
	}
	handler := StaticHandler(dist)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
		wantType   string
	}{
		{"root serves index", "/", http.StatusOK, indexHTML, "text/html; charset=utf-8"},
		{"client route falls back to index", "/p/some-slug", http.StatusOK, indexHTML, "text/html; charset=utf-8"},
		{"missing hashed asset is 404", "/assets/index-abc.js", http.StatusNotFound, "404 page not found\n", ""},
		{"present asset is served", "/assets/present.png", http.StatusOK, pngBody, ""},
		{"extensionless non-asset falls back to index", "/api/nope", http.StatusOK, indexHTML, "text/html; charset=utf-8"},
		{"missing file with extension is 404", "/foo.js", http.StatusNotFound, "404 page not found\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			body, err := io.ReadAll(rec.Result().Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(body); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if tt.wantType != "" {
				if got := rec.Header().Get("Content-Type"); got != tt.wantType {
					t.Fatalf("Content-Type = %q, want %q", got, tt.wantType)
				}
			}
		})
	}
}
