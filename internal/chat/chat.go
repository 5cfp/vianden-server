// Package chat handles text channels and their messages.
//
// Access rules (M5): every channel has a minimum role to SEE it and a minimum role to WRITE
// in it. A channel a user cannot see behaves as if it does not exist (ErrChannelNotFound,
// never "forbidden"), so its existence is not revealed.
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
	"github.com/5cfp/vianden-server/internal/files"
	"github.com/5cfp/vianden-server/internal/perm"
)

// Limits (also documented in docs/API.md).
const (
	MaxChannelNameLength  = 32
	MaxChannelTopicLength = 120
	MaxMessageLength      = 4000
	MaxAttachments        = 10
	DefaultPageSize       = 50
	MaxPageSize           = 100
	previewLength         = 100 // characters of the last message shown in the room list
)

var (
	ErrChannelNotFound  = errors.New("channel not found")
	ErrChannelNameTaken = errors.New("a channel with this name already exists")
	ErrMessageNotFound  = errors.New("message not found")
	// ErrReadOnly: the user can see the channel but not write in it.
	ErrReadOnly = errors.New("you cannot write in this channel")
)

// Channel is a text channel, with a preview of its newest message (nil if it has none).
type Channel struct {
	ID          int64
	Name        string
	Topic       string
	Type        string
	Position    int
	ViewRole    perm.Role // minimum role to see the channel and its messages
	SendRole    perm.Role // minimum role to write in it (never below ViewRole)
	LastMessage *Preview
	// For the viewer (only filled by ListChannels): the newest message they have read,
	// how many newer ones there are, and how many of those mention them (both max 100).
	LastReadID   int64
	UnreadCount  int
	MentionCount int
}

// CanView reports whether a user with this role may see the channel.
func (c Channel) CanView(r perm.Role) bool { return r.AtLeast(c.ViewRole) }

// CanSend reports whether a user with this role may write in the channel.
func (c Channel) CanSend(r perm.Role) bool { return c.CanView(r) && r.AtLeast(c.SendRole) }

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
	// Deleted: removed by its author or a moderator. Content is then "" (the text is
	// erased in the database).
	Deleted bool
	// EditedAt: when the author last changed the text; nil if never.
	EditedAt *time.Time
	// ReplyTo: the message this one answers (a short quote); nil if it is not a reply.
	ReplyTo *ReplyPreview
	// Mentions: who this message pings (@username, and the author of the message it
	// replies to). MentionsEveryone: it pinged @everyone (author allowed to).
	Mentions         []Author
	MentionsEveryone bool
	// Attachments: uploaded files sent with this message (none once it is deleted).
	Attachments []files.Attachment
}

// ReplyPreview is the quote shown above a reply.
type ReplyPreview struct {
	ID      int64
	Author  *Author // nil if the account no longer exists
	Content string  // at most 100 characters; "" if the original was deleted
	Deleted bool
}

type Author struct {
	ID          int64
	Username    string
	DisplayName string
}

// ChannelSettings is everything a manager can set on a channel.
type ChannelSettings struct {
	Name     string
	Topic    string
	ViewRole perm.Role // "" = member
	SendRole perm.Role // "" = member
}

// ChannelChanges: nil fields keep their current value.
type ChannelChanges struct {
	Name     *string
	Topic    *string
	ViewRole *perm.Role
	SendRole *perm.Role
}

type Service struct {
	pool    *pgxpool.Pool // for transactions
	queries *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, queries: db.New(pool)}
}

// ListChannels returns the channels the viewer may see, in room-list order.
func (s *Service) ListChannels(ctx context.Context, viewer accounts.User) ([]Channel, error) {
	rows, err := s.queries.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, 0, len(rows))
	for _, r := range rows {
		c := Channel{
			ID: r.ID, Name: r.Name, Topic: r.Topic, Type: r.Type, Position: int(r.Position),
			ViewRole: perm.Role(r.ViewRole), SendRole: perm.Role(r.SendRole),
		}
		if !c.CanView(viewer.Role) {
			continue
		}
		if r.HasLastMessage {
			p := &Preview{Content: truncate(r.LastContent, previewLength), CreatedAt: r.LastCreatedAt}
			if r.LastAuthor != nil {
				p.AuthorName = *r.LastAuthor
			}
			c.LastMessage = p
		}
		channels = append(channels, c)
	}

	// Unread counts for this viewer, matched to the channels by id.
	counts, err := s.queries.UnreadCounts(ctx, viewer.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]db.UnreadCountsRow, len(counts))
	for _, u := range counts {
		byID[u.ChannelID] = u
	}
	for i := range channels {
		u := byID[channels[i].ID]
		channels[i].LastReadID, channels[i].UnreadCount, channels[i].MentionCount = u.LastReadID, int(u.UnreadCount), int(u.MentionCount)
	}
	return channels, nil
}

// MarkRead records that the user has read the channel up to messageID (the marker only
// moves forward). Returns the new marker, for the user's other devices.
func (s *Service) MarkRead(ctx context.Context, by accounts.User, channelID, messageID int64) (int64, error) {
	if _, err := s.Channel(ctx, by, channelID); err != nil {
		return 0, err
	}
	return s.queries.MarkRead(ctx, db.MarkReadParams{UserID: by.ID, ChannelID: channelID, MessageID: messageID})
}

// Channel returns one channel, if the viewer may see it (used for access checks elsewhere).
func (s *Service) Channel(ctx context.Context, viewer accounts.User, id int64) (Channel, error) {
	c, err := s.queries.GetChannel(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, ErrChannelNotFound
	}
	if err != nil {
		return Channel{}, err
	}
	ch := toChannel(c)
	if !ch.CanView(viewer.Role) {
		return Channel{}, ErrChannelNotFound
	}
	return ch, nil
}

// ChannelAccess returns a channel's view and send roles, for the live hub (no user context).
func (s *Service) ChannelAccess(ctx context.Context, id int64) (view, send perm.Role, ok bool) {
	c, err := s.queries.GetChannel(ctx, id)
	if err != nil {
		return "", "", false
	}
	return perm.Role(c.ViewRole), perm.Role(c.SendRole), true
}

// CreateChannel adds a text channel at the end of the list.
func (s *Service) CreateChannel(ctx context.Context, by accounts.User, set ChannelSettings) (Channel, error) {
	if !by.Role.Has(perm.ManageChannels) {
		return Channel{}, accounts.ErrForbidden
	}
	set, err := cleanSettings(by, set)
	if err != nil {
		return Channel{}, err
	}
	c, err := s.queries.CreateChannel(ctx, db.CreateChannelParams{
		Name: set.Name, Topic: set.Topic, ViewRole: string(set.ViewRole), SendRole: string(set.SendRole),
	})
	if err != nil {
		return Channel{}, mapChannelError(err)
	}
	return toChannel(c), nil
}

// UpdateChannel changes a channel's settings. Managers can only change channels they can
// see, and cannot set access above their own role: otherwise an admin could "unhide" an
// owner-only channel for themselves.
// It also returns the PREVIOUS view role, so the caller can tell users who just lost access.
func (s *Service) UpdateChannel(ctx context.Context, by accounts.User, id int64, ch ChannelChanges) (Channel, perm.Role, error) {
	if !by.Role.Has(perm.ManageChannels) {
		return Channel{}, "", accounts.ErrForbidden
	}
	current, err := s.Channel(ctx, by, id)
	if err != nil {
		return Channel{}, "", err
	}

	set := ChannelSettings{Name: current.Name, Topic: current.Topic, ViewRole: current.ViewRole, SendRole: current.SendRole}
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
	} else if !set.SendRole.AtLeast(set.ViewRole) {
		// Only "who can see" was raised: raise "who can write" with it (nobody can write
		// in a channel they cannot see), instead of rejecting the change.
		set.SendRole = set.ViewRole
	}
	set, err = cleanSettings(by, set)
	if err != nil {
		return Channel{}, "", err
	}

	c, err := s.queries.UpdateChannel(ctx, db.UpdateChannelParams{
		ID: id, Name: set.Name, Topic: set.Topic, ViewRole: string(set.ViewRole), SendRole: string(set.SendRole),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, "", ErrChannelNotFound // deleted in the meantime
	}
	if err != nil {
		return Channel{}, "", mapChannelError(err)
	}
	return toChannel(c), current.ViewRole, nil
}

// DeleteChannel deletes a channel AND all its messages. This cannot be undone.
// Returns the deleted channel (its access roles decide who is told about it).
func (s *Service) DeleteChannel(ctx context.Context, by accounts.User, id int64) (Channel, error) {
	if !by.Role.Has(perm.ManageChannels) {
		return Channel{}, accounts.ErrForbidden
	}
	c, err := s.Channel(ctx, by, id) // only channels you can see
	if err != nil {
		return Channel{}, err
	}
	n, err := s.queries.DeleteChannel(ctx, id)
	if err != nil {
		return Channel{}, err
	}
	if n == 0 {
		return Channel{}, ErrChannelNotFound
	}
	return c, nil
}

// SendMessage posts a message to a channel as the given user. replyTo is the id of the
// message it answers, or 0 for a normal message.
func (s *Service) SendMessage(ctx context.Context, by accounts.User, channelID int64, content string, replyTo int64, attachments []int64) (Message, Channel, error) {
	attachments = unique(attachments)
	if len(attachments) > MaxAttachments {
		return Message{}, Channel{}, &accounts.ValidationError{Field: "attachments", Message: "at most 10 files per message"}
	}
	content, err := cleanContent(content, len(attachments) > 0)
	if err != nil {
		return Message{}, Channel{}, err
	}
	c, err := s.Channel(ctx, by, channelID)
	if err != nil {
		return Message{}, Channel{}, err
	}
	if !c.CanSend(by.Role) {
		return Message{}, Channel{}, ErrReadOnly
	}

	names, everyone := parseMentions(content)
	params := db.CreateMessageParams{ChannelID: channelID, AuthorID: &by.ID, Content: content,
		MentionsEveryone: canMentionEveryone(by, everyone)}
	var replyAuthor int64 // a reply pings the author of the original
	if replyTo != 0 {
		// The original must be in THIS channel. Otherwise someone could reply "into" a
		// channel they can see while quoting a message from one they cannot, and the quote
		// would leak its text to everyone here.
		author, err := s.queries.ReplyTarget(ctx, db.ReplyTargetParams{ID: replyTo, ChannelID: channelID})
		if errors.Is(err, pgx.ErrNoRows) {
			return Message{}, Channel{}, &accounts.ValidationError{Field: "reply_to", Message: "no such message in this channel"}
		}
		if err != nil {
			return Message{}, Channel{}, err
		}
		if author != nil {
			replyAuthor = *author
		}
		params.ReplyToID = &replyTo
	}

	// The message, who it pings, and "the sender has read up to here" are saved together.
	var id int64
	err = s.withTx(ctx, func(q *db.Queries) error {
		var err error
		if id, err = q.CreateMessage(ctx, params); err != nil {
			return err
		}
		if err := saveMentions(ctx, q, id, by, c, names, replyAuthor); err != nil {
			return err
		}
		if len(attachments) > 0 {
			// Only your own uploads that are not attached to anything yet.
			linked, err := q.LinkAttachments(ctx, db.LinkAttachmentsParams{MessageID: &id, Ids: attachments, UploaderID: &by.ID})
			if err != nil {
				return err
			}
			if len(linked) != len(attachments) {
				return &accounts.ValidationError{Field: "attachments", Message: "unknown upload, or already attached to a message"}
			}
		}
		_, err = q.MarkRead(ctx, db.MarkReadParams{UserID: by.ID, ChannelID: channelID, MessageID: id})
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation: deleted meanwhile
			return Message{}, Channel{}, ErrChannelNotFound
		}
		return Message{}, Channel{}, err
	}
	m, err := s.getMessage(ctx, channelID, id)
	return m, c, err
}

// EditMessage changes the text of one of the user's OWN messages (nobody can edit
// someone else's words, not even the owner). Like sending, it needs write access to the
// channel. Returns the updated message and its channel (for the live event).
func (s *Service) EditMessage(ctx context.Context, by accounts.User, channelID, messageID int64, content string) (Message, Channel, error) {
	c, err := s.Channel(ctx, by, channelID)
	if err != nil {
		return Message{}, Channel{}, err
	}
	if !c.CanSend(by.Role) {
		return Message{}, Channel{}, ErrReadOnly
	}
	current, err := s.getMessage(ctx, channelID, messageID)
	if err != nil {
		return Message{}, Channel{}, err
	}
	if current.Deleted {
		return Message{}, Channel{}, ErrMessageNotFound
	}
	if current.Author == nil || current.Author.ID != by.ID {
		return Message{}, Channel{}, accounts.ErrForbidden
	}
	// A message with files may end up with no text; one without must keep some.
	if content, err = cleanContent(content, len(current.Attachments) > 0); err != nil {
		return Message{}, Channel{}, err
	}

	// Mentions follow the new text (the reply ping stays: the message is still a reply).
	names, everyone := parseMentions(content)
	var replyAuthor int64
	if current.ReplyTo != nil && current.ReplyTo.Author != nil {
		replyAuthor = current.ReplyTo.Author.ID
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.EditMessage(ctx, db.EditMessageParams{ID: messageID, AuthorID: &by.ID, Content: content,
			MentionsEveryone: canMentionEveryone(by, everyone)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrMessageNotFound // deleted meanwhile
		}
		if err := q.DeleteMentions(ctx, messageID); err != nil {
			return err
		}
		return saveMentions(ctx, q, messageID, by, c, names, replyAuthor)
	})
	if err != nil {
		return Message{}, Channel{}, err
	}
	m, err := s.getMessage(ctx, channelID, messageID)
	return m, c, err
}

// getMessage loads one message (also deleted ones) of a channel.
func (s *Service) getMessage(ctx context.Context, channelID, messageID int64) (Message, error) {
	row, err := s.queries.GetMessage(ctx, db.GetMessageParams{ID: messageID, ChannelID: channelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, err
	}
	// GetMessageRow and ListMessagesRow have exactly the same fields (the queries select
	// the same columns), so Go allows converting one struct type into the other.
	msgs := []Message{toMessage(db.ListMessagesRow(row))}
	if err := s.decorate(ctx, msgs); err != nil {
		return Message{}, err
	}
	return msgs[0], nil
}

// toMessage turns a database row into a Message, including the reply quote.
func toMessage(r db.ListMessagesRow) Message {
	m := Message{
		ID: r.ID, ChannelID: r.ChannelID, Content: r.Content, CreatedAt: r.CreatedAt,
		Deleted: r.Deleted, EditedAt: r.EditedAt, MentionsEveryone: r.MentionsEveryone,
	}
	if r.AuthorID != nil && r.AuthorUsername != nil && r.AuthorDisplayName != nil {
		m.Author = &Author{ID: *r.AuthorID, Username: *r.AuthorUsername, DisplayName: *r.AuthorDisplayName}
	}
	if r.ReplyToID != nil && r.ReplyContent != nil { // ReplyContent is NULL only if the original row is gone
		q := &ReplyPreview{ID: *r.ReplyToID, Content: truncate(*r.ReplyContent, previewLength), Deleted: r.ReplyDeleted}
		if r.ReplyAuthorID != nil && r.ReplyAuthorUsername != nil && r.ReplyAuthorDisplayName != nil {
			q.Author = &Author{ID: *r.ReplyAuthorID, Username: *r.ReplyAuthorUsername, DisplayName: *r.ReplyAuthorDisplayName}
		}
		m.ReplyTo = q
	}
	return m
}

// ListMessages returns one page of a channel's history in chronological order (oldest first).
//
// before = 0 gets the newest messages; otherwise only messages with an id lower than
// `before` (older ones). hasMore tells whether even older messages exist.
//
// This is "keyset" pagination: it asks for "the 50 messages before id X", instead of
// "skip 5000 messages, then take 50" (offset pagination). It stays fast however long the
// history is, and new messages arriving meanwhile cannot shift the pages.
func (s *Service) ListMessages(ctx context.Context, viewer accounts.User, channelID, before int64, limit int) (msgs []Message, hasMore bool, err error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	limit = min(limit, MaxPageSize)

	if _, err := s.Channel(ctx, viewer, channelID); err != nil {
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
		msgs[len(rows)-1-i] = toMessage(r)
	}
	if err := s.decorate(ctx, msgs); err != nil {
		return nil, false, err
	}
	return msgs, hasMore, nil
}

func toChannel(c db.Channel) Channel {
	return Channel{
		ID: c.ID, Name: c.Name, Topic: c.Topic, Type: c.Type, Position: int(c.Position),
		ViewRole: perm.Role(c.ViewRole), SendRole: perm.Role(c.SendRole),
	}
}

func mapChannelError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "channels_name_lower_idx" {
		return ErrChannelNameTaken
	}
	return err
}

// cleanSettings validates channel settings. Access roles must be valid, writing needs at
// least the role for seeing, and neither may be above the manager's own role (no locking
// yourself out, no creating channels you could then not manage).
func cleanSettings(by accounts.User, set ChannelSettings) (ChannelSettings, error) {
	name, topic, err := cleanChannelFields(set.Name, set.Topic)
	if err != nil {
		return ChannelSettings{}, err
	}
	set.Name, set.Topic = name, topic
	if set.ViewRole == "" {
		set.ViewRole = perm.Member
	}
	if set.SendRole == "" {
		set.SendRole = set.ViewRole
	}
	if !set.ViewRole.Valid() {
		return ChannelSettings{}, &accounts.ValidationError{Field: "view_role", Message: "must be owner, admin, moderator, or member"}
	}
	if !set.SendRole.Valid() {
		return ChannelSettings{}, &accounts.ValidationError{Field: "send_role", Message: "must be owner, admin, moderator, or member"}
	}
	if !set.SendRole.AtLeast(set.ViewRole) {
		return ChannelSettings{}, &accounts.ValidationError{Field: "send_role", Message: "cannot be lower than view_role (you cannot write where you cannot read)"}
	}
	if set.ViewRole.Above(by.Role) || set.SendRole.Above(by.Role) {
		return ChannelSettings{}, accounts.ErrForbidden
	}
	return set, nil
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
// cleanContent: like cleanMessage, but a message with files may have no text at all.
func cleanContent(content string, hasFiles bool) (string, error) {
	if hasFiles && strings.TrimSpace(content) == "" {
		return "", nil
	}
	return cleanMessage(content)
}

func unique(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

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

// DeleteMessage erases a message's text (for real, in the database) and leaves a
// "message deleted" placeholder. Everyone may delete their OWN messages. Deleting someone
// else's needs the delete_messages permission, and the author must be below you (a
// moderator cannot delete an admin's message). Messages of deleted accounts can always be
// removed by someone with the permission.
// Returns the channel, so the caller can tell exactly the users who can see it.
func (s *Service) DeleteMessage(ctx context.Context, by accounts.User, channelID, messageID int64) (Channel, error) {
	c, err := s.Channel(ctx, by, channelID) // only in channels you can see
	if err != nil {
		return Channel{}, err
	}
	m, err := s.queries.GetMessageWithAuthor(ctx, db.GetMessageWithAuthorParams{ID: messageID, ChannelID: channelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, ErrMessageNotFound
	}
	if err != nil {
		return Channel{}, err
	}

	authorRole := perm.Role("") // deleted account: below everyone
	if m.AuthorRole != nil {
		authorRole = perm.Role(*m.AuthorRole)
	}
	own := m.AuthorID != nil && *m.AuthorID == by.ID
	if !own && !perm.CanActOn(by.Role, perm.DeleteMessages, authorRole) {
		return Channel{}, accounts.ErrForbidden
	}

	// The text is erased and the files go with it (their rows now; the files on disk at
	// the next cleanup, see files.Service.Cleanup). Downloads stop working at once.
	err = s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.DeleteMessage(ctx, db.DeleteMessageParams{ID: messageID, DeletedBy: &by.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrMessageNotFound // deleted by someone else meanwhile
		}
		return q.DeleteMessageAttachments(ctx, &messageID)
	})
	if err != nil {
		return Channel{}, err
	}
	return c, nil
}
