package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/files"
)

// Profiles changes your own profile (M6). The real server passes *accounts.Service.
type Profiles interface {
	UpdateDisplayName(ctx context.Context, userID int64, name string) (accounts.Member, error)
	GetMember(ctx context.Context, id int64) (accounts.Member, error)
}

// Avatars stores avatar pictures (M6). The real server passes *files.Service.
type Avatars interface {
	SetAvatar(ctx context.Context, userID int64, body io.Reader) error
	RemoveAvatar(ctx context.Context, userID int64) error
	OpenAvatar(key string) (io.ReadSeekCloser, error)
}

// avatarURL is where clients download an avatar; nil when the user has none.
func avatarURL(key string) *string {
	if key == "" {
		return nil
	}
	u := "/api/v1/avatars/" + key
	return &u
}

// profileChanged tells everyone about the new name or avatar (member.updated), and
// updates the name shown in "who is online".
func profileChanged(w http.ResponseWriter, rt Realtime, m accounts.Member) {
	rt.UpdateUserProfile(m.ID, m.DisplayName)
	rt.Broadcast("member.updated", toMemberResponse(m, false))
	writeJSON(w, http.StatusOK, struct {
		User userResponse `json:"user"`
	}{toUserResponse(m.User)})
}

func handleUpdateProfile(svc Profiles, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		var req struct {
			DisplayName string `json:"display_name"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		m, err := svc.UpdateDisplayName(r.Context(), s.User.ID, req.DisplayName)
		if err != nil {
			writeServiceError(w, logger, "update profile", err)
			return
		}
		profileChanged(w, rt, m)
	}
}

func handleSetAvatar(av Avatars, svc Profiles, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
		body := http.MaxBytesReader(w, r.Body, files.MaxAvatarSize+1)
		if err := av.SetAvatar(r.Context(), s.User.ID, body); err != nil {
			if errors.Is(err, files.ErrTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "avatars can be at most 5 MB")
				return
			}
			writeServiceError(w, logger, "set avatar", err)
			return
		}
		avatarChanged(r.Context(), w, svc, rt, logger, s.User.ID)
	}
}

func handleRemoveAvatar(av Avatars, svc Profiles, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		if err := av.RemoveAvatar(r.Context(), s.User.ID); err != nil {
			writeServiceError(w, logger, "remove avatar", err)
			return
		}
		avatarChanged(r.Context(), w, svc, rt, logger, s.User.ID)
	}
}

func avatarChanged(ctx context.Context, w http.ResponseWriter, svc Profiles, rt Realtime, logger *slog.Logger, userID int64) {
	m, err := svc.GetMember(ctx, userID)
	if err != nil {
		writeServiceError(w, logger, "load profile", err)
		return
	}
	profileChanged(w, rt, m)
}

func handleGetAvatar(av Avatars) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, _ accounts.Session) {
		f, err := av.OpenAvatar(r.PathValue("key"))
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "avatar not found")
			return
		}
		defer f.Close()
		h := w.Header()
		h.Set("Content-Type", "image/png") // always: the server drew it itself
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		// A new avatar gets a new key (a new URL), so an old one never changes.
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeContent(w, r, "", time.Time{}, f)
	}
}
