// Package web embeds the production browser client into the pons server.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The build writes the client to dist/app, which is not committed. The
// committed dist/.gitkeep keeps this pattern valid in a checkout without a
// build, so the server compiles without Node.
//
//go:embed all:dist
var assets embed.FS

const notBuiltPage = `<!doctype html>
<html lang="en">
<head><meta charset="UTF-8"><title>pons</title></head>
<body>
<h1>Web UI not built</h1>
<p>Run <code>make web-install</code> and <code>make web-build</code>, then rebuild and restart the server.</p>
</body>
</html>
`

// Handler serves the embedded single-page web client, or a page explaining
// how to build it when this binary was compiled without it.
func Handler() http.Handler {
	content, err := fs.Sub(assets, "dist/app")
	if err != nil {
		panic("web: embedded assets: " + err.Error())
	}
	return handler(content)
}

// handler serves content as a single-page app. Unknown paths fall back to the
// entry point so client-side routes can be added without changing the server.
func handler(content fs.FS) http.Handler {
	if _, err := fs.Stat(content, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(notBuiltPage))
		})
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
