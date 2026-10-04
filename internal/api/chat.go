package api

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/chat"
)

// Chat is the channel and message logic the API needs.
// The real server passes a *chat.Service; tests pass a fake.
type Chat interface {
	ListChannels(ctx context.Context) ([]chat.Channel, error)
	CreateChannel(ctx context.Context, by accounts.User, name, topic string) (chat.Channel, error)
	UpdateChannel(ctx context.Context, by accounts.User, id int64, name, topic *string) (chat.Channel, error)
	DeleteChannel(ctx context.Context, by accounts.User, id int64) error
	SendMessage(ctx context.Context, by accounts.User, channelID int64, content string) (chat.Message, error)
	ListMessages(ctx context.Context, channelID, before int64, limit int) ([]chat.Message, bool, error)
}

type channelResponse struct {
	ID          int64            `json:"id"`
	Name        string           `json:"name"`
	Topic       string           `json:"topic"`
	Type        string           `json:"type"`
	Position    int              `json:"position"`
	LastMessage *previewResponse `json:"last_message"` // null when the channel has no messages
}

type previewResponse struct {
	AuthorName string    `json:"author_name"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
}

type authorResponse struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type messageResponse struct {
	ID        int64           `json:"id"`
	ChannelID int64           `json:"channel_id"`
	Author    *authorResponse `json:"author"` // null if the account no longer exists
	Content   string          `json:"content"`
	CreatedAt time.Time       `json:"created_at"`
}

func toChannelResponse(c chat.Channel) channelResponse {
	r := channelResponse{ID: c.ID, Name: c.Name, Topic: c.Topic, Type: c.Type, Position: c.Position}
	if p := c.LastMessage; p != nil {
		r.LastMessage = &previewResponse{AuthorName: p.AuthorName, Content: p.Content, CreatedAt: p.CreatedAt.UTC()}
	}
	return r
}

func toMessageResponse(m chat.Message) messageResponse {
	r := messageResponse{ID: m.ID, ChannelID: m.ChannelID, Content: m.Content, CreatedAt: m.CreatedAt.UTC()}
	if a := m.Author; a != nil {
		r.Author = &authorResponse{ID: a.ID, Username: a.Username, DisplayName: a.DisplayName}
	}
	return r
}

func handleListChannels(svc Chat, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, _ accounts.Session) {
		channels, err := svc.ListChannels(r.Context())
		if err != nil {
			writeServiceError(w, logger, "list channels", err)
			return
		}
		resp := struct {
			Channels []channelResponse `json:"channels"`
		}{Channels: make([]channelResponse, len(channels))}
		for i, c := range channels {
			resp.Channels[i] = toChannelResponse(c)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type channelRequest struct {
	// Pointers tell "not sent" (nil) apart from "sent as empty" (""), so PATCH can update one field.
	Name  *string `json:"name"`
	Topic *string `json:"topic"`
}

func handleCreateChannel(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		var req channelRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		var name, topic string
		if req.Name != nil {
			name = *req.Name
		}
		if req.Topic != nil {
			topic = *req.Topic
		}
		c, err := svc.CreateChannel(r.Context(), s.User, name, topic)
		if err != nil {
			writeServiceError(w, logger, "create channel", err)
			return
		}
		logger.Info("channel created", "channel_id", c.ID, "by_user_id", s.User.ID)
		rt.Broadcast("channel.created", toChannelResponse(c))
		writeJSON(w, http.StatusCreated, struct {
			Channel channelResponse `json:"channel"`
		}{toChannelResponse(c)})
	}
}

func handleUpdateChannel(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
			return
		}
		var req channelRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		c, err := svc.UpdateChannel(r.Context(), s.User, id, req.Name, req.Topic)
		if err != nil {
			writeServiceError(w, logger, "update channel", err)
			return
		}
		logger.Info("channel updated", "channel_id", c.ID, "by_user_id", s.User.ID)
		rt.Broadcast("channel.updated", toChannelResponse(c))
		writeJSON(w, http.StatusOK, struct {
			Channel channelResponse `json:"channel"`
		}{toChannelResponse(c)})
	}
}

func handleDeleteChannel(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
			return
		}
		if err := svc.DeleteChannel(r.Context(), s.User, id); err != nil {
			writeServiceError(w, logger, "delete channel", err)
			return
		}
		logger.Info("channel deleted", "channel_id", id, "by_user_id", s.User.ID)
		rt.Broadcast("channel.deleted", map[string]int64{"id": id})
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleListMessages(svc Chat, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, _ accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
			return
		}

		q := r.URL.Query()
		before, err := optionalInt(q.Get("before"), 1, math.MaxInt64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_before", "before must be a message id")
			return
		}
		limit, err := optionalInt(q.Get("limit"), 1, chat.MaxPageSize)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
			return
		}

		msgs, hasMore, err := svc.ListMessages(r.Context(), id, before, int(limit))
		if err != nil {
			writeServiceError(w, logger, "list messages", err)
			return
		}
		resp := struct {
			Messages []messageResponse `json:"messages"`
			HasMore  bool              `json:"has_more"`
		}{Messages: make([]messageResponse, len(msgs)), HasMore: hasMore}
		for i, m := range msgs {
			resp.Messages[i] = toMessageResponse(m)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleSendMessage(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
			return
		}
		var req struct {
			Content string `json:"content"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		// Message text is never logged: it is private conversation.
		m, err := svc.SendMessage(r.Context(), s.User, id, req.Content)
		if err != nil {
			writeServiceError(w, logger, "send message", err)
			return
		}
		resp := toMessageResponse(m)
		rt.Broadcast("message.created", resp) // to everyone, the sender's other devices too
		writeJSON(w, http.StatusCreated, struct {
			Message messageResponse `json:"message"`
		}{resp})
	}
}

// pathID reads a positive integer id from the URL path, e.g. {id} in /channels/{id}.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// optionalInt parses an optional query parameter: "" gives 0, otherwise it must be in [lo, hi].
func optionalInt(s string, lo, hi int64) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, strconv.ErrRange
	}
	return n, nil
}
