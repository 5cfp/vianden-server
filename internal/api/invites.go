package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/chat"
)

type createInviteRequest struct {
	// Both optional: 0 (or missing) means the server default.
	MaxUses        int `json:"max_uses"`
	ExpiresInHours int `json:"expires_in_hours"`
}

type inviteResponse struct {
	ID        int64     `json:"id"`
	CreatedBy int64     `json:"created_by"`
	MaxUses   int       `json:"max_uses"`
	Uses      int       `json:"uses"`
	ExpiresAt time.Time `json:"expires_at"` // JSON: RFC 3339, e.g. "2026-10-11T12:00:00Z"
	CreatedAt time.Time `json:"created_at"`
}

type createInviteResponse struct {
	Invite inviteResponse `json:"invite"`
	Code   string         `json:"code"`
}

type listInvitesResponse struct {
	Invites []inviteResponse `json:"invites"`
}

func toInviteResponse(i accounts.Invite) inviteResponse {
	return inviteResponse{
		ID: i.ID, CreatedBy: i.CreatedBy, MaxUses: i.MaxUses, Uses: i.Uses,
		ExpiresAt: i.ExpiresAt.UTC(), CreatedAt: i.CreatedAt.UTC(),
	}
}

func handleCreateInvite(svc Accounts, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		var req createInviteRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		inv, code, err := svc.CreateInvite(r.Context(), s.User, req.MaxUses, req.ExpiresInHours)
		if err != nil {
			writeServiceError(w, logger, "create invite", err)
			return
		}
		logger.Info("invite created", "invite_id", inv.ID, "by_user_id", s.User.ID, "max_uses", inv.MaxUses)
		writeJSON(w, http.StatusCreated, createInviteResponse{Invite: toInviteResponse(inv), Code: code})
	}
}

func handleListInvites(svc Accounts, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		invites, err := svc.ListInvites(r.Context(), s.User)
		if err != nil {
			writeServiceError(w, logger, "list invites", err)
			return
		}
		resp := listInvitesResponse{Invites: make([]inviteResponse, len(invites))} // [] not null when empty
		for i, inv := range invites {
			resp.Invites[i] = toInviteResponse(inv)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleDeleteInvite(svc Accounts, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", accounts.ErrInviteNotFound.Error())
			return
		}
		if err := svc.DeleteInvite(r.Context(), s.User, id); err != nil {
			writeServiceError(w, logger, "delete invite", err)
			return
		}
		logger.Info("invite deleted", "invite_id", id, "by_user_id", s.User.ID)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeServiceError maps errors from the accounts and chat services to API errors.
func writeServiceError(w http.ResponseWriter, logger *slog.Logger, action string, err error) {
	var invalid *accounts.ValidationError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, "invalid_"+invalid.Field, invalid.Error())
	case errors.Is(err, accounts.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, accounts.ErrInviteNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, chat.ErrChannelNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, chat.ErrChannelNameTaken):
		writeError(w, http.StatusConflict, "channel_name_taken", err.Error())
	default:
		logger.Error(action+" failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
