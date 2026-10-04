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
	"strings"
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
	Register(ctx context.Context, in accounts.RegisterInput) (accounts.AuthResult, error)
	Login(ctx context.Context, username, password string) (accounts.AuthResult, error)
	Authenticate(ctx context.Context, token string) (accounts.Session, error)
	Logout(ctx context.Context, sessionID int64) error

	CreateInvite(ctx context.Context, by accounts.User, maxUses, expiresInHours int) (accounts.Invite, string, error)
	ListInvites(ctx context.Context, by accounts.User) ([]accounts.Invite, error)
	DeleteInvite(ctx context.Context, by accounts.User, id int64) error
}

// Realtime pushes live events to connected clients. The real server passes a
// *realtime.Hub; tests pass a fake (or nothing: then events are simply dropped).
type Realtime interface {
	Serve(w http.ResponseWriter, r *http.Request, s accounts.Session)
	Broadcast(eventType string, data any)
	EndSession(sessionID int64)
}

// Deps are the things the API handlers depend on.
type Deps struct {
	ServerName string
	DB         Pinger
	Accounts   Accounts
	Chat       Chat
	Realtime   Realtime
	Logger     *slog.Logger
}

// NewHandler returns the HTTP handler for the whole API.
func NewHandler(d Deps) http.Handler {
	if d.Realtime == nil {
		d.Realtime = noRealtime{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", handleHealth(d.DB, d.Logger))
	mux.HandleFunc("GET /api/v1/info", handleInfo(d.ServerName))

	// Login and register share one limit per client: 10 attempts, then 1 more every 6 seconds.
	authLimit := newIPRateLimiter(10, 10)
	mux.HandleFunc("POST /api/v1/register", authLimit.limit(handleRegister(d.Accounts, d.Logger)))
	mux.HandleFunc("POST /api/v1/login", authLimit.limit(handleLogin(d.Accounts, d.Logger)))

	// These need a valid session token.
	mux.HandleFunc("POST /api/v1/logout", requireAuth(d.Accounts, d.Logger, handleLogout(d.Accounts, d.Realtime, d.Logger)))
	mux.HandleFunc("GET /api/v1/me", requireAuth(d.Accounts, d.Logger, handleMe))
	mux.HandleFunc("POST /api/v1/invites", requireAuth(d.Accounts, d.Logger, handleCreateInvite(d.Accounts, d.Logger)))
	mux.HandleFunc("GET /api/v1/invites", requireAuth(d.Accounts, d.Logger, handleListInvites(d.Accounts, d.Logger)))
	mux.HandleFunc("DELETE /api/v1/invites/{id}", requireAuth(d.Accounts, d.Logger, handleDeleteInvite(d.Accounts, d.Logger)))

	// Channels and messages (any logged-in user; managing channels: owner only, checked in the service).
	mux.HandleFunc("GET /api/v1/channels", requireAuth(d.Accounts, d.Logger, handleListChannels(d.Chat, d.Logger)))
	mux.HandleFunc("POST /api/v1/channels", requireAuth(d.Accounts, d.Logger, handleCreateChannel(d.Chat, d.Realtime, d.Logger)))
	mux.HandleFunc("PATCH /api/v1/channels/{id}", requireAuth(d.Accounts, d.Logger, handleUpdateChannel(d.Chat, d.Realtime, d.Logger)))
	mux.HandleFunc("DELETE /api/v1/channels/{id}", requireAuth(d.Accounts, d.Logger, handleDeleteChannel(d.Chat, d.Realtime, d.Logger)))
	mux.HandleFunc("GET /api/v1/channels/{id}/messages", requireAuth(d.Accounts, d.Logger, handleListMessages(d.Chat, d.Logger)))
	// Sending: 10 messages at once, then 1 per second, per user (stops spam and runaway clients).
	sendLimit := newIPRateLimiter(60, 10)
	mux.HandleFunc("POST /api/v1/channels/{id}/messages", requireAuth(d.Accounts, d.Logger, sendLimit.limitUser(handleSendMessage(d.Chat, d.Realtime, d.Logger))))

	// Live events (WebSocket). Login is checked BEFORE the upgrade, with the normal Bearer header.
	mux.HandleFunc("GET /api/v1/ws", requireAuth(d.Accounts, d.Logger, d.Realtime.Serve))

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
		return errBadRequest{jsonErrorMessage(err)}
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

// noRealtime is used when no Realtime is given (tests): events go nowhere.
type noRealtime struct{}

func (noRealtime) Serve(w http.ResponseWriter, _ *http.Request, _ accounts.Session) {
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}
func (noRealtime) Broadcast(string, any) {}
func (noRealtime) EndSession(int64)      {}

// jsonErrorMessage turns a JSON decoding error into a message for the client. Go's own
// messages mention internal type names (e.g. "Go struct field registerRequest.username"),
// which tell an attacker about the code; these say only what the client needs to fix.
func jsonErrorMessage(err error) string {
	var tooBig *http.MaxBytesError
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooBig):
		return "request body is too large"
	case errors.Is(err, io.EOF):
		return "request body is empty"
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return "request body is not valid JSON"
	case errors.As(err, &wrongType):
		return fmt.Sprintf("field %q has the wrong type", wrongType.Field)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// The field name comes from the client's own request, so repeating it is safe.
		return "unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field ")
	default:
		return "request body is not valid JSON"
	}
}

// forLog shortens client-provided text before logging it, so a huge value cannot fill
// the log (the body may be up to 64 KiB). slog already escapes line breaks and quotes.
func forLog(s string) string {
	const maxRunes = 64
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}
