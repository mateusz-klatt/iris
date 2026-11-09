// Package webui serves the embedded operator console.
package webui

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Available reports whether a console entry point is embedded. It does not
// establish that its assets are complete or current with the source.
func Available() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler serves the console. A path with no embedded file returns the shell,
// so a refresh away from / still loads the app. Mount API routes elsewhere:
// this handler does not know them.
func Handler() http.Handler {
	return handlerFrom(dist)
}

// handlerFrom serves console files from bundle/dist. Tests pass a MapFS so an
// embedded asset can be fetched and a bundle without index.html can report why.
func handlerFrom(bundle fs.FS) http.Handler {
	root, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic(err)
	}
	return handler(root)
}

func handler(root fs.FS) http.Handler {
	index, indexErr := fs.ReadFile(root, "index.html")
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if indexErr != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			if r.Method != http.MethodHead {
				_, _ = fmt.Fprintln(w, "Iris console is missing from this binary. Run make build from the source checkout to build the console and binary together.")
			}
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" && name != "index.html" {
			if _, err := fs.Stat(root, name); err != nil {
				http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
