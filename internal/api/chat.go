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
	"github.com/5cfp/vianden-server/internal/perm"
)

// Chat is the channel and message logic the API needs.
// The real server passes a *chat.Service; tests pass a fake.
type Chat interface {
	ListChannels(ctx context.Context, viewer accounts.User) ([]chat.Channel, error)
	CreateChannel(ctx context.Context, by accounts.User, set chat.ChannelSettings) (chat.Channel, error)
	UpdateChannel(ctx context.Context, by accounts.User, id int64, ch chat.ChannelChanges) (chat.Channel, perm.Role, error)
	DeleteChannel(ctx context.Context, by accounts.User, id int64) (chat.Channel, error)
	SendMessage(ctx context.Context, by accounts.User, channelID int64, content string, replyTo int64, attachments []int64) (chat.Message, chat.Channel, error)
	Channel(ctx context.Context, viewer accounts.User, id int64) (chat.Channel, error)
	EditMessage(ctx context.Context, by accounts.User, channelID, messageID int64, content string) (chat.Message, chat.Channel, error)
	MarkRead(ctx context.Context, by accounts.User, channelID, messageID int64) (int64, error)
	ListMessages(ctx context.Context, viewer accounts.User, channelID, before int64, limit int) ([]chat.Message, bool, error)
	DeleteMessage(ctx context.Context, by accounts.User, channelID, messageID int64) (chat.Channel, error)
}

type channelResponse struct {
	ID          int64            `json:"id"`
	Name        string           `json:"name"`
	Topic       string           `json:"topic"`
	Type        string           `json:"type"`
	Position    int              `json:"position"`
	ViewRole    string           `json:"view_role"`    // minimum role to see the channel
	SendRole    string           `json:"send_role"`    // minimum role to write in it
	LastMessage *previewResponse `json:"last_message"` // null when the channel has no messages
	// For you: newest message you read, and how many newer ones (and mentions of you) exist.
	// Counts stop at 100. Only meaningful in GET /channels (0 in events).
	LastReadID   int64 `json:"last_read_id"`
	UnreadCount  int   `json:"unread_count"`
	MentionCount int   `json:"mention_count"`
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
	// Deleted by its author or a moderator: content is "" and should be shown as "Message deleted".
	Deleted          bool                 `json:"deleted"`
	EditedAt         *time.Time           `json:"edited_at"`         // null if never edited
	ReplyTo          *replyResponse       `json:"reply_to"`          // null if not a reply
	Mentions         []authorResponse     `json:"mentions"`          // who it pings (never null)
	MentionsEveryone bool                 `json:"mentions_everyone"` // it pinged @everyone
	Attachments      []attachmentResponse `json:"attachments"`       // never null
}

// replyResponse is the quote of the message a reply answers.
type replyResponse struct {
	ID      int64           `json:"id"`
	Author  *authorResponse `json:"author"`  // null if the account no longer exists
	Content string          `json:"content"` // at most 100 characters (+ "…"); "" if deleted
	Deleted bool            `json:"deleted"`
}

func toChannelResponse(c chat.Channel) channelResponse {
	r := channelResponse{
		ID: c.ID, Name: c.Name, Topic: c.Topic, Type: c.Type, Position: c.Position,
		ViewRole: string(c.ViewRole), SendRole: string(c.SendRole),
		LastReadID: c.LastReadID, UnreadCount: c.UnreadCount, MentionCount: c.MentionCount,
	}
	if p := c.LastMessage; p != nil {
		r.LastMessage = &previewResponse{AuthorName: p.AuthorName, Content: p.Content, CreatedAt: p.CreatedAt.UTC()}
	}
	return r
}

func toMessageResponse(m chat.Message) messageResponse {
	r := messageResponse{ID: m.ID, ChannelID: m.ChannelID, Content: m.Content, CreatedAt: m.CreatedAt.UTC(), Deleted: m.Deleted,
		Mentions: make([]authorResponse, len(m.Mentions)), MentionsEveryone: m.MentionsEveryone,
		Attachments: make([]attachmentResponse, len(m.Attachments))}
	for i, a := range m.Attachments {
		r.Attachments[i] = toAttachmentResponse(a)
	}
	for i, a := range m.Mentions {
		r.Mentions[i] = authorResponse{ID: a.ID, Username: a.Username, DisplayName: a.DisplayName}
	}
	if a := m.Author; a != nil {
		r.Author = &authorResponse{ID: a.ID, Username: a.Username, DisplayName: a.DisplayName}
	}
	if m.EditedAt != nil {
		t := m.EditedAt.UTC()
		r.EditedAt = &t
	}
	if q := m.ReplyTo; q != nil {
		r.ReplyTo = &replyResponse{ID: q.ID, Content: q.Content, Deleted: q.Deleted}
		if a := q.Author; a != nil {
			r.ReplyTo.Author = &authorResponse{ID: a.ID, Username: a.Username, DisplayName: a.DisplayName}
		}
	}
	return r
}

func handleListChannels(svc Chat, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		channels, err := svc.ListChannels(r.Context(), s.User)
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
	Name     *string `json:"name"`
	Topic    *string `json:"topic"`
	ViewRole *string `json:"view_role"`
	SendRole *string `json:"send_role"`
	Type     *string `json:"type"` // "text" or "voice": create only
}

func (req channelRequest) changes() chat.ChannelChanges {
	ch := chat.ChannelChanges{Name: req.Name, Topic: req.Topic}
	if req.ViewRole != nil {
		r := perm.Role(*req.ViewRole)
		ch.ViewRole = &r
	}
	if req.SendRole != nil {
		r := perm.Role(*req.SendRole)
		ch.SendRole = &r
	}
	return ch
}

func handleCreateChannel(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		var req channelRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		ch := req.changes()
		var set chat.ChannelSettings
		if ch.Name != nil {
			set.Name = *ch.Name
		}
		if ch.Topic != nil {
			set.Topic = *ch.Topic
		}
		if ch.ViewRole != nil {
			set.ViewRole = *ch.ViewRole
		}
		if ch.SendRole != nil {
			set.SendRole = *ch.SendRole
		}
		if req.Type != nil {
			set.Type = *req.Type
		}
		c, err := svc.CreateChannel(r.Context(), s.User, set)
		if err != nil {
			writeServiceError(w, logger, "create channel", err)
			return
		}
		logger.Info("channel created", "channel_id", c.ID, "by_user_id", s.User.ID)
		// Only people who may see the new channel learn that it exists.
		rt.BroadcastWhere("channel.created", toChannelResponse(c), c.CanView)
		writeJSON(w, http.StatusCreated, struct {
			Channel channelResponse `json:"channel"`
		}{toChannelResponse(c)})
	}
}

func handleUpdateChannel(svc Chat, rt Realtime, vc Voice, logger *slog.Logger) authedHandler {
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
		if req.Type != nil {
			writeError(w, http.StatusBadRequest, "invalid_type", "a channel's type cannot be changed")
			return
		}
		c, previousView, err := svc.UpdateChannel(r.Context(), s.User, id, req.changes())
		if err != nil {
			writeServiceError(w, logger, "update channel", err)
			return
		}
		logger.Info("channel updated", "channel_id", c.ID, "by_user_id", s.User.ID)
		rt.BroadcastWhere("channel.updated", toChannelResponse(c), c.CanView)
		// Users who could see it before but not any more: for them it is gone.
		rt.BroadcastWhere("channel.deleted", map[string]int64{"id": c.ID}, func(role perm.Role) bool {
			return role.AtLeast(previousView) && !c.CanView(role)
		})
		vc.ChannelChanged(r.Context(), c.ID) // new rules for who may join or speak
		writeJSON(w, http.StatusOK, struct {
			Channel channelResponse `json:"channel"`
		}{toChannelResponse(c)})
	}
}

func handleDeleteChannel(svc Chat, rt Realtime, vc Voice, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
			return
		}
		c, err := svc.DeleteChannel(r.Context(), s.User, id)
		if err != nil {
			writeServiceError(w, logger, "delete channel", err)
			return
		}
		logger.Info("channel deleted", "channel_id", id, "by_user_id", s.User.ID)
		rt.BroadcastWhere("channel.deleted", map[string]int64{"id": id}, c.CanView)
		vc.ChannelDeleted(id)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleListMessages(svc Chat, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
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

		msgs, hasMore, err := svc.ListMessages(r.Context(), s.User, id, before, int(limit))
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
			Content     string  `json:"content"`
			ReplyTo     *int64  `json:"reply_to"`    // optional: id of the message this answers
			Attachments []int64 `json:"attachments"` // optional: ids from POST /attachments
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		// Message text is never logged: it is private conversation.
		m, ch, err := svc.SendMessage(r.Context(), s.User, id, req.Content, deref(req.ReplyTo), req.Attachments)
		if err != nil {
			writeServiceError(w, logger, "send message", err)
			return
		}
		resp := toMessageResponse(m)
		// To everyone who may see the channel (the sender's other devices too).
		rt.BroadcastWhere("message.created", resp, ch.CanView)
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

func handleDeleteMessage(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		channelID, ok1 := pathID(r, "id")
		messageID, ok2 := pathID(r, "mid")
		if !ok1 || !ok2 {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrMessageNotFound.Error())
			return
		}
		ch, err := svc.DeleteMessage(r.Context(), s.User, channelID, messageID)
		if err != nil {
			writeServiceError(w, logger, "delete message", err)
			return
		}
		logger.Info("message deleted", "message_id", messageID, "channel_id", channelID, "by_user_id", s.User.ID)
		rt.BroadcastWhere("message.deleted", map[string]int64{"id": messageID, "channel_id": channelID}, ch.CanView)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// deref returns *p, or 0 for nil (an optional id that was not sent).
func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func handleEditMessage(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		channelID, ok1 := pathID(r, "id")
		messageID, ok2 := pathID(r, "mid")
		if !ok1 || !ok2 {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrMessageNotFound.Error())
			return
		}
		var req struct {
			Content string `json:"content"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		m, ch, err := svc.EditMessage(r.Context(), s.User, channelID, messageID, req.Content)
		if err != nil {
			writeServiceError(w, logger, "edit message", err)
			return
		}
		resp := toMessageResponse(m)
		rt.BroadcastWhere("message.updated", resp, ch.CanView)
		writeJSON(w, http.StatusOK, struct {
			Message messageResponse `json:"message"`
		}{resp})
	}
}

func handleMarkRead(svc Chat, rt Realtime, logger *slog.Logger) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s accounts.Session) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", chat.ErrChannelNotFound.Error())
			return
		}
		var req struct {
			MessageID int64 `json:"message_id"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.MessageID < 0 {
			writeError(w, http.StatusBadRequest, "invalid_message_id", "message_id must be a message id")
			return
		}
		last, err := svc.MarkRead(r.Context(), s.User, id, req.MessageID)
		if err != nil {
			writeServiceError(w, logger, "mark read", err)
			return
		}
		// The user's other devices clear their badge too.
		rt.SendToUser(s.User.ID, "channel.read", map[string]int64{"channel_id": id, "last_read_id": last})
		w.WriteHeader(http.StatusNoContent)
	}
}
