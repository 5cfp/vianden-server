package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
)

// memberResponse is a user in the member list. The ban fields are only filled in for
// viewers who may ban (moderation details are not everyone's business).
type memberResponse struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Role        string     `json:"role"`
	Banned      *bool      `json:"banned,omitempty"`
	BannedAt    *time.Time `json:"banned_at,omitempty"`
	BanReason   *string    `json:"ban_reason,omitempty"`
}

func toMemberResponse(m accounts.Member, showBan bool) memberResponse {
	r := memberResponse{ID: m.ID, Username: m.Username, DisplayName: m.DisplayName, Role: string(m.Role)}
	if showBan {
		banned := m.BannedAt != nil
		r.Banned = &banned
		if banned {
			at := m.BannedAt.UTC()
			r.BannedAt = &at
			r.BanReason = &m.BanReason
		}
	}
	return r
}

func handleListMembers(svc Accounts, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		members, err := svc.ListMembers(r.Context())
		if err != nil {
			writeServiceError(w, logger, "list members", err)
			return
		}
		showBan := s.User.Role.Has(perm.BanMembers)
		resp := struct {
			Users []memberResponse `json:"users"`
		}{Users: make([]memberResponse, len(members))}
		for i, m := range members {
			resp.Users[i] = toMemberResponse(m, showBan)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleSetRole(svc Accounts, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", accounts.ErrUserNotFound.Error())
			return
		}
		var req struct {
			Role string `json:"role"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		m, err := svc.SetRole(r.Context(), s.User, id, perm.Role(req.Role))
		if err != nil {
			writeServiceError(w, logger, "set role", err)
			return
		}
		logger.Info("role changed", "user_id", m.ID, "role", m.Role, "by_user_id", s.User.ID)
		rt.UpdateUserRole(m.ID, m.Role)
		resp := toMemberResponse(m, false)
		rt.Broadcast("member.updated", resp)
		writeJSON(w, http.StatusOK, struct {
			User memberResponse `json:"user"`
		}{toMemberResponse(m, s.User.Role.Has(perm.BanMembers))})
	}
}

func handleKick(svc Accounts, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", accounts.ErrUserNotFound.Error())
			return
		}
		if err := svc.Kick(r.Context(), s.User, id); err != nil {
			writeServiceError(w, logger, "kick", err)
			return
		}
		logger.Info("user kicked", "user_id", id, "by_user_id", s.User.ID)
		rt.EndUser(id) // their open connections close now, not at their next request
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleBan(svc Accounts, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", accounts.ErrUserNotFound.Error())
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := svc.Ban(r.Context(), s.User, id, req.Reason); err != nil {
			writeServiceError(w, logger, "ban", err)
			return
		}
		logger.Info("user banned", "user_id", id, "by_user_id", s.User.ID)
		rt.EndUser(id)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleUnban(svc Accounts, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", accounts.ErrUserNotFound.Error())
			return
		}
		if err := svc.Unban(r.Context(), s.User, id); err != nil {
			writeServiceError(w, logger, "unban", err)
			return
		}
		logger.Info("user unbanned", "user_id", id, "by_user_id", s.User.ID)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
