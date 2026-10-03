// Package api contains the REST API handlers. Every route is documented in docs/API.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/buildinfo"
)

// Pinger is anything that can check the database connection.
// The real server passes a *pgxpool.Pool; tests pass a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Accounts is the account logic the API needs.
// The real server passes an *accounts.Service; tests pass a fake.
type Accounts interface {
	Register(ctx context.Context, in accounts.RegisterInput) (accounts.RegisterResult, error)
}

// Deps are the things the API handlers depend on.
type Deps struct {
	ServerName string
	DB         Pinger
	Accounts   Accounts
	Logger     *slog.Logger
}

// NewHandler returns the HTTP handler for the whole API.
func NewHandler(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", handleHealth(d.DB, d.Logger))
	mux.HandleFunc("GET /api/v1/info", handleInfo(d.ServerName))
	mux.HandleFunc("POST /api/v1/register", handleRegister(d.Accounts, d.Logger))
	// Anything that matches no route above gets a JSON 404 in the standard error format.
	mux.HandleFunc("/", handleNotFound)

	return recoverPanics(d.Logger, mux)
}

type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

func handleHealth(db Pinger, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			// The reason goes to the log only; clients just learn that the database is down.
			logger.Error("health check: database unreachable", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "unavailable", Database: "unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Database: "ok"})
	}
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

// maxBodyBytes limits request bodies, so a client cannot exhaust memory with a huge request.
const maxBodyBytes = 64 << 10 // 64 KiB

// errBadRequest is returned by decodeJSON; its message is safe to show to the client.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

// decodeJSON reads a JSON request body into dst. It is strict on purpose:
// unknown fields, a second JSON value, or a body over 64 KiB are all rejected.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errBadRequest{"request body is too large"}
		}
		return errBadRequest{fmt.Sprintf("invalid JSON body: %v", err)}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errBadRequest{"request body must contain a single JSON object"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Tell browsers not to guess a different content type (defense against MIME sniffing).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Responses may contain session tokens or private data: never store them in any cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client may have disconnected; nothing useful to do
}
