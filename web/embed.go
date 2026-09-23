// Package web embeds the production browser client into the pons server.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist/*
var assets embed.FS

// Handler serves the single-page web client. Unknown paths fall back to the
// entry point so client-side routes can be added without changing the server.
func Handler() http.Handler {
	content, err := fs.Sub(assets, "dist")
	if err != nil {
		panic("web: embedded assets: " + err.Error())
	}
	files := http.FileServer(http.FS(content))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if info, statErr := fs.Stat(content, name); statErr != nil || info.IsDir() {
			name = "index.html"
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		request := r.Clone(r.Context())
		request.URL.Path = "/" + name
		if name == "index.html" {
			request.URL.Path = "/"
		}
		files.ServeHTTP(w, request)
	})
}
