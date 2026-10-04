package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/5cfp/vianden-server/internal/accounts"
)

type registerRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	InviteCode  string `json:"invite_code"`
}

type userResponse struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsOwner     bool   `json:"is_owner"`
}

type sessionResponse struct {
	User  userResponse `json:"user"`
	Token string       `json:"token"`
}

func toUserResponse(u accounts.User) userResponse {
	return userResponse{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, IsOwner: u.IsOwner}
}

func handleRegister(svc Accounts, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		result, err := svc.Register(r.Context(), accounts.RegisterInput{
			Username:    req.Username,
			DisplayName: req.DisplayName,
			Password:    req.Password,
			InviteCode:  req.InviteCode,
		})

		var invalid *accounts.ValidationError
		switch {
		case err == nil:
			// Never log the token or the password.
			logger.Info("user registered", "user_id", result.User.ID, "username", result.User.Username, "owner", result.User.IsOwner)
			writeJSON(w, http.StatusCreated, sessionResponse{User: toUserResponse(result.User), Token: result.SessionToken})
		case errors.As(err, &invalid):
			writeError(w, http.StatusBadRequest, "invalid_"+invalid.Field, invalid.Error())
		case errors.Is(err, accounts.ErrInvalidInvite):
			writeError(w, http.StatusForbidden, "invalid_invite", err.Error())
		case errors.Is(err, accounts.ErrInvalidSetupToken):
			writeError(w, http.StatusForbidden, "invalid_setup_token", err.Error())
		case errors.Is(err, accounts.ErrUsernameTaken):
			writeError(w, http.StatusConflict, "username_taken", err.Error())
		default:
			logger.Error("register failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleLogin(svc Accounts, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		result, err := svc.Login(r.Context(), req.Username, req.Password)
		switch {
		case err == nil:
			logger.Info("user logged in", "user_id", result.User.ID, "ip", clientIP(r))
			writeJSON(w, http.StatusOK, sessionResponse{User: toUserResponse(result.User), Token: result.SessionToken})
		case errors.Is(err, accounts.ErrInvalidCredentials):
			// Logged so the owner can spot password guessing. slog escapes the values,
			// so a username with line breaks cannot forge fake log lines.
			logger.Warn("failed login", "username", req.Username, "ip", clientIP(r))
			writeError(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
		default:
			logger.Error("login failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}
	}
}

// authedHandler is a handler that runs only for a logged-in user.
type authedHandler func(w http.ResponseWriter, r *http.Request, s accounts.Session)

// requireAuth checks the "Authorization: Bearer <token>" header before calling next.
// Every reason for failure (no header, wrong format, unknown, expired, or revoked token)
// gets the same 401 answer, so the response reveals nothing about which tokens exist.
func requireAuth(svc Accounts, logger *slog.Logger, next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeUnauthorized(w)
			return
		}

		session, err := svc.Authenticate(r.Context(), token)
		if errors.Is(err, accounts.ErrUnauthenticated) {
			writeUnauthorized(w)
			return
		}
		if err != nil {
			logger.Error("authentication failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		next(w, r, session)
	}
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
// The word "Bearer" is case-insensitive (RFC 6750).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func writeUnauthorized(w http.ResponseWriter) {
	// Tells the client which kind of authentication this server expects.
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", accounts.ErrUnauthenticated.Error())
}

func handleLogout(svc Accounts, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		if err := svc.Logout(r.Context(), s.ID); err != nil {
			logger.Error("logout failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		rt.EndSession(s.ID) // live connections of this session close too
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

type meResponse struct {
	User userResponse `json:"user"`
}

func handleMe(w http.ResponseWriter, r *http.Request, s accounts.Session) {
	writeJSON(w, http.StatusOK, meResponse{User: toUserResponse(s.User)})
}
