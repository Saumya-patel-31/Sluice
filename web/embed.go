// Package web embeds the Sluice dashboard so the control plane ships as a
// single binary with no asset directory to deploy alongside it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var assets embed.FS

// FS returns the dashboard's asset filesystem rooted at the static directory.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// Unreachable: the embed directive guarantees the directory exists,
		// and a failure here would be a build-time problem, not a runtime one.
		panic("web: embedded assets are missing: " + err.Error())
	}
	return sub
}

// Handler serves the dashboard.
//
// Unknown paths fall back to index.html so the client-side view router owns
// the URL space and a deep link such as /decisions survives a page reload.
// Requests under /api are never reached here; the mux matches those first.
func Handler() http.Handler {
	files := http.FS(FS())
	fileServer := http.FileServer(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		f, err := files.Open("/" + path)
		if err != nil {
			serveIndex(w, r)
			return
		}
		st, statErr := f.Stat()
		_ = f.Close()
		if statErr != nil || st.IsDir() {
			serveIndex(w, r)
			return
		}

		// Assets carry no content hash in their filenames, so they must be
		// revalidated rather than cached by age: an operator who upgrades the
		// binary and then reads a stale dashboard is looking at a lie about
		// their traffic. The whole bundle is well under 100 KB, so the cost of
		// revalidating is negligible next to that risk.
		if strings.HasSuffix(path, ".css") || strings.HasSuffix(path, ".js") ||
			strings.HasSuffix(path, ".svg") || strings.HasSuffix(path, ".woff2") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		http.Error(w, "dashboard assets unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
