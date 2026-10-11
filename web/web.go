// Package web serves the embedded Drydock frontend.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFiles embed.FS

// Handler serves the frontend from web/static. Unknown non-API paths fall
// back to index.html so the kanban board always boots.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path[1:], "/") && !strings.Contains(r.URL.Path, ".") {
			r.URL.Path = "/"
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
