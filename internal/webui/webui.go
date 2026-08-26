// Package webui embeds the Vue production build and provides an HTTP handler
// with single-page application fallback routing.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// embedded contains the output produced by `npm run build` in frontend/.
//
//go:embed dist
var embedded embed.FS

var distFS = mustSub(embedded, "dist")

func mustSub(root fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(root, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Assets returns the immutable embedded production assets rooted at dist/.
func Assets() fs.FS {
	return distFS
}

// Handler serves embedded assets and falls back to index.html for client-side
// routes. API paths and missing files with extensions remain regular 404s.
// It can be mounted at / or used from Gin via gin.WrapH(webui.Handler()).
func Handler() http.Handler {
	files := http.FileServer(http.FS(distFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		requested := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if requested == "." {
			requested = ""
		}

		if requested != "" {
			if info, err := fs.Stat(distFS, requested); err == nil && !info.IsDir() {
				setAssetCacheHeader(w, requested)
				files.ServeHTTP(w, r)
				return
			}
		}

		if strings.HasPrefix(requested, "api/") || path.Ext(requested) != "" {
			http.NotFound(w, r)
			return
		}

		clone := r.Clone(r.Context())
		clone.URL.Path = "/"
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, clone)
	})
}

func setAssetCacheHeader(w http.ResponseWriter, name string) {
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
