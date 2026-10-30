package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HTTPHandler serves stateless Streamable HTTP at /mcp with local Host/Origin
// validation. An optional token protects standalone adapters; embedded callers
// must apply their application's authentication when token is empty.
func (s *Server) HTTPHandler(port, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		host, requestPort, err := net.SplitHostPort(r.Host)
		if err != nil || !loopbackHost(host) || requestPort != port {
			http.Error(w, "invalid MCP Host", http.StatusForbidden)
			return
		}
		if origins := r.Header.Values("Origin"); len(origins) > 0 {
			origin, err := url.Parse(origins[0])
			if len(origins) != 1 || err != nil || origin.Scheme != "http" || origin.Host != r.Host ||
				origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
				http.Error(w, "invalid MCP Origin", http.StatusForbidden)
				return
			}
		}
		if token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid MCP bearer token", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "only POST is supported; no SSE stream or sessions", http.StatusMethodNotAllowed)
			return
		}
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		if !acceptsJSON(r.Header.Values("Accept")) {
			http.Error(w, "Accept must include application/json", http.StatusNotAcceptable)
			return
		}
		switch r.Header.Get("MCP-Protocol-Version") {
		case "", "2025-03-26", "2025-06-18", protocolVersion:
		default:
			http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			status := http.StatusBadRequest
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "read MCP body: "+err.Error(), status)
			return
		}
		res := s.dispatch(r.Context(), body)
		if res == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		data, err := json.Marshal(res)
		if err != nil {
			log.Printf("encode MCP HTTP response: %v", err)
			http.Error(w, "encode MCP response failed", http.StatusInternalServerError)
			return
		}
		status := http.StatusOK
		if res.Error != nil && (res.Error.Code == -32700 || res.Error.Code == -32600) {
			status = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write(data); err != nil {
			log.Printf("write MCP HTTP response: %v", err)
		}
	})
}

func acceptsJSON(headers []string) bool {
	for _, header := range headers {
		for _, value := range strings.Split(header, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
			if err == nil && params["q"] != "0" && (mediaType == "application/json" || mediaType == "*/*") {
				return true
			}
		}
	}
	return false
}
