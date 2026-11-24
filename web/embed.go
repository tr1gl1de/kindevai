// Package web embeds the built React Dashboard so the Control Plane ships as a single binary.
//
// `npm run build` writes the app into dist/app. Without a build, the committed dist/placeholder.html
// is served so `go build` and `go test` work on a fresh checkout without Node.js.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the filesystem to serve at the site root: the built app when present, otherwise
// a placeholder holding a single index.html. The boolean reports whether the real app was found.
func Assets() (fs.FS, bool) {
	if app, err := fs.Sub(dist, "dist/app"); err == nil {
		if _, err := fs.Stat(app, "index.html"); err == nil {
			return app, true
		}
	}
	return placeholderFS{}, false
}

// placeholderFS exposes dist/placeholder.html as index.html.
type placeholderFS struct{}

func (placeholderFS) Open(name string) (fs.File, error) {
	if name != "index.html" {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return dist.Open("dist/placeholder.html")
}
