// Package webui serves the embedded operator console.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the console. A path with no embedded file returns the shell,
// so a refresh away from / still loads the app. Mount API routes elsewhere:
// this handler does not know them.
func Handler() http.Handler {
	return handlerFrom(dist)
}

// handlerFrom serves console files from bundle/dist. Tests pass a MapFS so an
// embedded asset can be fetched and a bundle without index.html can 404.
func handlerFrom(bundle fs.FS) http.Handler {
	root, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic(err)
	}
	return handler(root)
}

func handler(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" && name != "index.html" {
			if _, err := fs.Stat(root, name); err != nil {
				serveIndex(w, r, root)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	body, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(body))
}
