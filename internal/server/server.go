// Package server wires the Control Plane HTTP API and the embedded Dashboard.
package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Options configures the HTTP handler.
type Options struct {
	Version string
	Assets  fs.FS // site root with index.html; see web.Assets
	Logger  *slog.Logger
}

// New returns the root HTTP handler.
func New(opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": opts.Version})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
	})
	mux.Handle("/", spa(opts.Assets))
	return securityHeaders(mux)
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client is gone if this fails; nothing useful to do
}

// ErrorBody is the JSON shape of every API error.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// WriteError writes a JSON error with a stable machine code and a human message.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: msg})
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
