package api

import (
	"errors"
	"log/slog"
	"net/http"

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
