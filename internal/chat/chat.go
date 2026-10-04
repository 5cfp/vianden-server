// Package chat handles text channels and their messages.
package chat

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/db"
)

// Limits (also documented in docs/API.md).
const (
	MaxChannelNameLength  = 32
	MaxChannelTopicLength = 120
	MaxMessageLength      = 4000
	DefaultPageSize       = 50
	MaxPageSize           = 100
	previewLength         = 100 // characters of the last message shown in the room list
)

var (
	ErrChannelNotFound  = errors.New("channel not found")
	ErrChannelNameTaken = errors.New("a channel with this name already exists")
)

// Channel is a text channel, with a preview of its newest message (nil if it has none).
type Channel struct {
	ID          int64
	Name        string
	Topic       string
	Type        string
	Position    int
	LastMessage *Preview
}

// Preview is a shortened view of a channel's newest message, for the room list.
type Preview struct {
	AuthorName string // display name; "" if the author's account no longer exists
	Content    string // at most 100 characters
	CreatedAt  time.Time
}

// Message is one chat message. Author is nil if the account no longer exists.
type Message struct {
	ID        int64
	ChannelID int64
	Author    *Author
	Content   string
	CreatedAt time.Time
}

type Author struct {
	ID          int64
	Username    string
	DisplayName string
}

type Service struct {
	queries *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{queries: db.New(pool)}
}

// canManageChannels is the permission check. In M2 only the owner; roles arrive in M5.
func canManageChannels(u accounts.User) bool {
	return u.IsOwner
}

// ListChannels returns every channel, in room-list order. Any logged-in user may call it
// (per-channel permissions arrive in M5).
func (s *Service) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.queries.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, len(rows))
	for i, r := range rows {
		channels[i] = Channel{ID: r.ID, Name: r.Name, Topic: r.Topic, Type: r.Type, Position: int(r.Position)}
		if r.HasLastMessage {
			p := &Preview{Content: truncate(r.LastContent, previewLength), CreatedAt: r.LastCreatedAt}
			if r.LastAuthor != nil {
				p.AuthorName = *r.LastAuthor
			}
			channels[i].LastMessage = p
		}
	}
	return channels, nil
}

// CreateChannel adds a text channel at the end of the list.
func (s *Service) CreateChannel(ctx context.Context, by accounts.User, name, topic string) (Channel, error) {
	if !canManageChannels(by) {
		return Channel{}, accounts.ErrForbidden
	}
	name, topic, err := cleanChannelFields(name, topic)
	if err != nil {
		return Channel{}, err
	}
	c, err := s.queries.CreateChannel(ctx, db.CreateChannelParams{Name: name, Topic: topic})
	if err != nil {
		return Channel{}, mapChannelError(err)
	}
	return toChannel(c), nil
}

// UpdateChannel renames a channel and/or changes its topic. A nil argument keeps the current value.
func (s *Service) UpdateChannel(ctx context.Context, by accounts.User, id int64, name, topic *string) (Channel, error) {
	if !canManageChannels(by) {
		return Channel{}, accounts.ErrForbidden
	}
	current, err := s.queries.GetChannel(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, ErrChannelNotFound
	}
	if err != nil {
		return Channel{}, err
	}

	newName, newTopic := current.Name, current.Topic
	if name != nil {
		newName = *name
	}
	if topic != nil {
		newTopic = *topic
	}
	newName, newTopic, err = cleanChannelFields(newName, newTopic)
	if err != nil {
		return Channel{}, err
	}

	c, err := s.queries.UpdateChannel(ctx, db.UpdateChannelParams{ID: id, Name: newName, Topic: newTopic})
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, ErrChannelNotFound // deleted in the meantime
	}
	if err != nil {
		return Channel{}, mapChannelError(err)
	}
	return toChannel(c), nil
}

// DeleteChannel deletes a channel AND all its messages. This cannot be undone.
func (s *Service) DeleteChannel(ctx context.Context, by accounts.User, id int64) error {
	if !canManageChannels(by) {
		return accounts.ErrForbidden
	}
	n, err := s.queries.DeleteChannel(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

// SendMessage posts a message to a channel as the given user.
func (s *Service) SendMessage(ctx context.Context, by accounts.User, channelID int64, content string) (Message, error) {
	content, err := cleanMessage(content)
	if err != nil {
		return Message{}, err
	}

	m, err := s.queries.CreateMessage(ctx, db.CreateMessageParams{ChannelID: channelID, AuthorID: &by.ID, Content: content})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation: no such channel
			return Message{}, ErrChannelNotFound
		}
		return Message{}, err
	}
	return Message{
		ID: m.ID, ChannelID: m.ChannelID, Content: m.Content, CreatedAt: m.CreatedAt,
		Author: &Author{ID: by.ID, Username: by.Username, DisplayName: by.DisplayName},
	}, nil
}

// ListMessages returns one page of a channel's history in chronological order (oldest first).
//
// before = 0 gets the newest messages; otherwise only messages with an id lower than
// `before` (older ones). hasMore tells whether even older messages exist.
//
// This is "keyset" pagination: it asks for "the 50 messages before id X", instead of
// "skip 5000 messages, then take 50" (offset pagination). It stays fast however long the
// history is, and new messages arriving meanwhile cannot shift the pages.
func (s *Service) ListMessages(ctx context.Context, channelID, before int64, limit int) (msgs []Message, hasMore bool, err error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	limit = min(limit, MaxPageSize)

	if _, err := s.queries.GetChannel(ctx, channelID); errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrChannelNotFound
	} else if err != nil {
		return nil, false, err
	}

	params := db.ListMessagesParams{ChannelID: channelID, RowLimit: int32(limit + 1)} // +1: is there more?
	if before > 0 {
		params.Before = &before
	}
	rows, err := s.queries.ListMessages(ctx, params)
	if err != nil {
		return nil, false, err
	}

	hasMore = len(rows) > limit
	rows = rows[:min(len(rows), limit)]

	// The query returns newest first; the client wants oldest first.
	msgs = make([]Message, len(rows))
	for i, r := range rows {
		m := Message{ID: r.ID, ChannelID: r.ChannelID, Content: r.Content, CreatedAt: r.CreatedAt}
		if r.AuthorID != nil && r.AuthorUsername != nil && r.AuthorDisplayName != nil {
			m.Author = &Author{ID: *r.AuthorID, Username: *r.AuthorUsername, DisplayName: *r.AuthorDisplayName}
		}
		msgs[len(rows)-1-i] = m
	}
	return msgs, hasMore, nil
}

func toChannel(c db.Channel) Channel {
	return Channel{ID: c.ID, Name: c.Name, Topic: c.Topic, Type: c.Type, Position: int(c.Position)}
}

func mapChannelError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "channels_name_lower_idx" {
		return ErrChannelNameTaken
	}
	return err
}

// cleanChannelFields trims and validates a channel name and topic.
func cleanChannelFields(name, topic string) (string, string, error) {
	name = strings.TrimSpace(name)
	topic = strings.TrimSpace(topic)

	if n := utf8.RuneCountInString(name); n < 1 || n > MaxChannelNameLength {
		return "", "", &accounts.ValidationError{Field: "name", Message: "must be 1-32 characters"}
	}
	if !isPlainText(name, false) {
		return "", "", &accounts.ValidationError{Field: "name", Message: "contains invisible or control characters"}
	}
	if utf8.RuneCountInString(topic) > MaxChannelTopicLength {
		return "", "", &accounts.ValidationError{Field: "topic", Message: "must be at most 120 characters"}
	}
	if !isPlainText(topic, false) {
		return "", "", &accounts.ValidationError{Field: "topic", Message: "contains invisible or control characters"}
	}
	return name, topic, nil
}

// cleanMessage validates message text. Line breaks are allowed; Windows line endings become "\n".
func cleanMessage(content string) (string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		return "", &accounts.ValidationError{Field: "content", Message: "must not be empty"}
	}
	if !utf8.ValidString(content) || !isPlainText(content, true) {
		return "", &accounts.ValidationError{Field: "content", Message: "contains control characters"}
	}
	if utf8.RuneCountInString(content) > MaxMessageLength {
		return "", &accounts.ValidationError{Field: "content", Message: "must be at most 4000 characters"}
	}
	return content, nil
}

// isPlainText rejects control characters (except newlines and tabs in messages) and, in
// names, invisible formatting characters. Messages may contain formatting characters,
// because some languages and emoji sequences legitimately need them.
func isPlainText(s string, isMessage bool) bool {
	for _, r := range s {
		if isMessage && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
		if !isMessage && unicode.Is(unicode.Cf, r) && r != 0x200D {
			return false
		}
	}
	return true
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}
