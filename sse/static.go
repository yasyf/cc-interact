package sse

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// StaticHandler serves a single-page app from dist via http.FileServerFS. A miss
// falls back to index.html only for a client-route-shaped path (clientRoute) so
// deep links load the app while a missing asset 404s honestly; root "/" serves
// index. Opt-in: the consumer mounts it on the catch-all "/", below the pattern
// mux's /events and REST routes.
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
			if !clientRoute(p) {
				http.NotFound(w, r)
				return
			}
		}
		serveIndex(w, dist)
	})
}

// clientRoute reports whether p (leading slash trimmed) is shaped like an SPA
// deep link rather than a real file: no extension on its last segment and not
// under assets/. Anything with an extension or under assets/ names a file that is
// simply absent, so a miss on it 404s.
func clientRoute(p string) bool {
	if strings.HasPrefix(p, "assets/") {
		return false
	}
	return path.Ext(p) == ""
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
