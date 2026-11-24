// Package httpx holds HTTP helpers shared by all bounded contexts' HTTP adapters.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
)

// ErrorBody is the JSON shape of every API error. Field is set for validation errors.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client is gone if this fails; nothing useful to do
}

// WriteError writes a JSON error with a stable machine code and a human message.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: msg})
}

// Allow wraps a handler so other methods get a JSON 405 with an Allow header.
func Allow(methods []string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(methods, r.Method) {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		h(w, r)
	})
}

// MaxBodyBytes bounds JSON request bodies.
const MaxBodyBytes = 16 << 10

// DecodeJSON reads a JSON object into dst. It requires Content-Type application/json (which a cross-site
// HTML form cannot send without a CORS preflight), bounds the size and rejects unknown fields. On failure it
// writes the error response and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		// Decoder errors name Go types and fields; keep them out of responses.
		WriteError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body or unknown field")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, "invalid_json", "trailing data after JSON object")
		return false
	}
	return true
}

// ClientIP is the TCP peer address. Proxy headers (X-Forwarded-For, Forwarded, X-Real-IP) are ignored on
// purpose: they are client-controlled and must never grant trust or bypass limits.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
