package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// HTTPServer wraps the MCPServer with a streamable-HTTP transport and
// bearer-token authentication. Bind it to 127.0.0.1 by default and front
// it with the OS firewall if you ever expose it more broadly.
type HTTPServer struct {
	mcp   *MCPServer
	token string // bearer token clients must present
}

func NewHTTPServer(mcp *MCPServer, token string) *HTTPServer {
	return &HTTPServer{mcp: mcp, token: token}
}

// ListenAndServe binds to addr and starts serving. Blocks until the server
// errors out.
func (h *HTTPServer) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.HandleFunc("/", h.requireAuth(h.handleRPC))
	h.mcp.log("HTTP listening on %s (auth: bearer token)", addr)
	return http.ListenAndServe(addr, mux)
}

func (h *HTTPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Health endpoint is unauthenticated so a host-side healthcheck can
	// poke it without the token.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *HTTPServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" {
			// Server started in unauthenticated mode (--allow-no-auth).
			// We still want to be loud about it.
			next(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		got := auth[len(prefix):]
		// constant-time compare
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (h *HTTPServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	h.mcp.log("http request: %s (id=%v)", req.Method, req.ID)

	// Create streaming writer for tool calls that support it
	var sw *StreamingResponseWriter
	if flusher, ok := w.(http.Flusher); ok {
		sw = &StreamingResponseWriter{W: w, Flusher: flusher}
	}

	resp := h.mcp.HandleRequestStreaming(&req, sw)

	// If resp is nil, the handler already wrote the response (streaming mode)
	if resp == nil {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.Error != nil {
		h.mcp.log("error: %s", resp.Error.Message)
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] write error: %v\n", err)
	}
}
