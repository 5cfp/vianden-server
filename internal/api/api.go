// Package api contains the REST API handlers. Every route is documented in docs/API.md.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/5cfp/vianden-server/internal/buildinfo"
)

// NewHandler returns the HTTP handler for the whole API.
func NewHandler(serverName string, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", handleHealth)
	mux.HandleFunc("GET /api/v1/info", handleInfo(serverName))
	// Anything that matches no route above gets a JSON 404 in the standard error format.
	mux.HandleFunc("/", handleNotFound)

	return recoverPanics(logger, mux)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type infoResponse struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}

func handleInfo(serverName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, infoResponse{
			Name:            serverName,
			Version:         buildinfo.Version,
			ProtocolVersion: buildinfo.ProtocolVersion,
		})
	}
}

func handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}

// recoverPanics catches a panic in any handler, logs it, and answers 500.
// The panic details go only to the server log, never to the client:
// stack traces can reveal internals that help an attacker.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // net/http uses this panic on purpose to abort a response
			}
			// Log the path only, not the query string: it may contain secrets later.
			logger.Error("panic in HTTP handler",
				"method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// errorResponse is the standard error format for every endpoint (see docs/API.md).
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Tell browsers not to guess a different content type (defense against MIME sniffing).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client may have disconnected; nothing useful to do
}
