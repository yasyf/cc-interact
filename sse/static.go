package sse

import (
	"io/fs"
	"net/http"
	"strings"
)

// StaticHandler serves a single-page app from dist: real files (hashed assets,
// index.html) are served with their correct Content-Type via http.FileServerFS;
// any other path is a client-side route and falls back to index.html so deep
// links load the app. It is opt-in — the consumer owns the embed and mounts this
// on the catch-all "/" of the server's Mux, where Go's pattern mux gives the more
// specific /events and REST routes precedence so it never shadows them.
func StaticHandler(dist fs.FS) http.Handler {
	fileServer := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if f, err := dist.Open(p); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, dist)
	})
}

func serveIndex(w http.ResponseWriter, dist fs.FS) {
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "spa shell missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}
