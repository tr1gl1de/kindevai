// Package server wires the Control Plane HTTP API and the embedded Dashboard.
package server

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Options configures the HTTP handler.
type Options struct {
	Version string
	Assets  fs.FS // site root with index.html; see web.Assets
	Logger  *slog.Logger
	Modules []Module // bounded-context HTTP adapters
	// Middleware wraps all routes (authentication, CSRF); security headers are applied outside it.
	Middleware func(http.Handler) http.Handler
}

// Module registers a bounded context's API routes (under /api/).
type Module interface {
	Register(mux *http.ServeMux)
}

// New returns the root HTTP handler.
func New(opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/health", httpx.Allow([]string{http.MethodGet, http.MethodHead}, func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": opts.Version})
	}))
	for _, m := range opts.Modules {
		m.Register(mux)
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
	})
	mux.Handle("/", spa(opts.Assets))
	var h http.Handler = mux
	if opts.Middleware != nil {
		h = opts.Middleware(h)
	}
	return securityHeaders(h)
}

// spa serves static files from assets and falls back to index.html for client-side routes.
func spa(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		st, err := fs.Stat(assets, name)
		switch {
		case err == nil && !st.IsDir() && name != "index.html":
			if strings.HasPrefix(name, "assets/") { // Vite emits content-hashed names here
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, assets, name)
			return
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		case path.Ext(name) != "" && name != "index.html":
			// A missing file with an extension is a broken asset link, not a client route.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		serveIndex(w, r, assets)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	b, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "dashboard not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// contentSecurityPolicy allows only same-origin resources. Framing is limited to the Telegram web client,
// which hosts the Mini App in an iframe; native Telegram clients use a webview and are unaffected.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'self' https://web.telegram.org"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
