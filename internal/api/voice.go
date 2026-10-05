package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/chat"
	"github.com/5cfp/vianden-server/internal/perm"
	"github.com/5cfp/vianden-server/internal/voice"
)

// Voice is what the API needs from the voice service (M7). Signaling itself goes over
// the WebSocket (the hub passes it to voice directly).
type Voice interface {
	Available() bool
	List(ctx context.Context, viewer accounts.User) []voice.ChannelState
	Disconnect(by accounts.User, channelID, userID int64) error
	SetServerMute(by accounts.User, channelID, userID int64, muted bool) error
	UserChanged(userID int64)
	ChannelChanged(ctx context.Context, channelID int64)
	ChannelDeleted(channelID int64)
}

// noVoice is used when the server runs without voice.
type noVoice struct{}

func (noVoice) Available() bool { return false }
func (noVoice) List(context.Context, accounts.User) []voice.ChannelState {
	return []voice.ChannelState{}
}
func (noVoice) Disconnect(accounts.User, int64, int64) error          { return voice.ErrUnavailable }
func (noVoice) SetServerMute(accounts.User, int64, int64, bool) error { return voice.ErrUnavailable }
func (noVoice) UserChanged(int64)                                     {}
func (noVoice) ChannelChanged(context.Context, int64)                 {}
func (noVoice) ChannelDeleted(int64)                                  {}

// withVoice: when the hub learns about a new role or name, voice re-checks the user's
// session too (e.g. a demoted user may lose the right to speak).
type withVoice struct {
	Realtime
	v Voice
}

func (w withVoice) UpdateUserRole(userID int64, role perm.Role) {
	w.Realtime.UpdateUserRole(userID, role)
	w.v.UserChanged(userID)
}

func (w withVoice) UpdateUserProfile(userID int64, displayName string) {
	w.Realtime.UpdateUserProfile(userID, displayName)
	w.v.UserChanged(userID)
}

func handleListVoice(v Voice) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		writeJSON(w, http.StatusOK, struct {
			Available bool                 `json:"available"`
			Channels  []voice.ChannelState `json:"channels"`
		}{v.Available(), v.List(r.Context(), s.User)})
	}
}

// voiceTarget reads {id} (channel) and {uid} (user) and checks the channel is visible.
func voiceTarget(w http.ResponseWriter, r *http.Request, svc Chat, s accounts.Session) (channelID, userID int64, ok bool) {
	channelID, ok1 := pathID(r, "id")
	userID, ok2 := pathID(r, "uid")
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return 0, 0, false
	}
	if _, err := svc.Channel(r.Context(), s.User, channelID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
		return 0, 0, false
	}
	return channelID, userID, true
}

func writeVoiceError(w http.ResponseWriter, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, voice.ErrNotInVoice):
		writeError(w, http.StatusNotFound, "not_in_voice", err.Error())
	case errors.Is(err, voice.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "voice_unavailable", err.Error())
	default:
		writeServiceError(w, logger, "voice moderation", err)
	}
}

func handleVoiceDisconnect(svc Chat, v Voice, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		channelID, userID, ok := voiceTarget(w, r, svc, s)
		if !ok {
			return
		}
		if err := v.Disconnect(s.User, channelID, userID); err != nil {
			writeVoiceError(w, logger, err)
			return
		}
		logger.Info("voice disconnect", "channel_id", channelID, "user_id", userID, "by_user_id", s.User.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleVoiceMute(svc Chat, v Voice, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		channelID, userID, ok := voiceTarget(w, r, svc, s)
		if !ok {
			return
		}
		var req struct {
			Muted bool `json:"muted"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := v.SetServerMute(s.User, channelID, userID, req.Muted); err != nil {
			writeVoiceError(w, logger, err)
			return
		}
		logger.Info("voice server mute", "channel_id", channelID, "user_id", userID, "muted", req.Muted, "by_user_id", s.User.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}
