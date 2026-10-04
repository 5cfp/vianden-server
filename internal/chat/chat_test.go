package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/testdb"
)

var ctx = context.Background()

// setup returns a service on an empty test database with an owner and a member account.
func setup(t *testing.T) (*Service, *pgxpool.Pool, accounts.User, accounts.User) {
	t.Helper()
	pool := testdb.New(t)
	owner := addUser(t, pool, "osama", "Osama", true)
	member := addUser(t, pool, "friend", "Friend", false)
	return NewService(pool), pool, owner, member
}

func addUser(t *testing.T, pool *pgxpool.Pool, username, display string, owner bool) accounts.User {
	t.Helper()
	u := accounts.User{Username: username, DisplayName: display, IsOwner: owner}
	err := pool.QueryRow(ctx, "INSERT INTO users (username, display_name, password_hash, is_owner) VALUES ($1, $2, 'x', $3) RETURNING id",
		username, display, owner).Scan(&u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustCreate(t *testing.T, s *Service, owner accounts.User, name string) Channel {
	t.Helper()
	c, err := s.CreateChannel(ctx, owner, name, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func validationField(err error) string {
	var ve *accounts.ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return ""
}

func ptr(s string) *string { return &s }

// ---- channels ----

func TestCreateAndListChannels(t *testing.T) {
	s, _, owner, _ := setup(t)

	a, err := s.CreateChannel(ctx, owner, "  Games ", " fun stuff ")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Games" || a.Topic != "fun stuff" || a.Type != "text" {
		t.Errorf("unexpected channel %+v (name and topic should be trimmed)", a)
	}
	b := mustCreate(t, s, owner, "Homework")
	if b.Position != a.Position+1 {
		t.Errorf("positions %d, %d: new channels should go to the end", a.Position, b.Position)
	}

	list, err := s.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "Games" || list[1].Name != "Homework" || list[0].LastMessage != nil {
		t.Errorf("unexpected list %+v", list)
	}
}

func TestOnlyOwnerManagesChannels(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")

	if _, err := s.CreateChannel(ctx, member, "Mine", ""); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("member create: %v, want ErrForbidden", err)
	}
	if _, err := s.UpdateChannel(ctx, member, c.ID, ptr("Hacked"), nil); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("member rename: %v, want ErrForbidden", err)
	}
	if err := s.DeleteChannel(ctx, member, c.ID); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("member delete: %v, want ErrForbidden", err)
	}
	var name string
	pool.QueryRow(ctx, "SELECT name FROM channels WHERE id = $1", c.ID).Scan(&name)
	if name != "General" {
		t.Errorf("channel name is %q: the member changed it", name)
	}
}

func TestChannelNameRules(t *testing.T) {
	s, _, owner, _ := setup(t)
	mustCreate(t, s, owner, "Games")

	cases := map[string]struct{ name, topic, field string }{
		"empty":          {"   ", "", "name"},
		"too long":       {strings.Repeat("x", 33), "", "name"},
		"control char":   {"bad\nname", "", "name"},
		"bidi override":  {"evil" + string(rune(0x202E)) + "txt", "", "name"},
		"topic too long": {"Ok", strings.Repeat("t", 121), "topic"},
	}
	for label, c := range cases {
		if _, err := s.CreateChannel(ctx, owner, c.name, c.topic); validationField(err) != c.field {
			t.Errorf("%s: err = %v, want %s validation error", label, err, c.field)
		}
	}
	if _, err := s.CreateChannel(ctx, owner, "GAMES", ""); !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("duplicate name (other case): %v, want ErrChannelNameTaken", err)
	}
	if _, err := s.CreateChannel(ctx, owner, "Spiele 🎮 العاب", ""); err != nil {
		t.Errorf("any language and emoji should be allowed: %v", err)
	}
	if _, err := s.CreateChannel(ctx, owner, strings.Repeat("é", 32), ""); err != nil {
		t.Errorf("32 characters (64 bytes) should be allowed: %v", err)
	}
}

func TestUpdateChannel(t *testing.T) {
	s, _, owner, _ := setup(t)
	c := mustCreate(t, s, owner, "Games")
	mustCreate(t, s, owner, "Homework")

	renamed, err := s.UpdateChannel(ctx, owner, c.ID, ptr("Gaming"), nil)
	if err != nil || renamed.Name != "Gaming" || renamed.Topic != "" {
		t.Fatalf("rename: %+v, %v", renamed, err)
	}
	withTopic, err := s.UpdateChannel(ctx, owner, c.ID, nil, ptr("All the games"))
	if err != nil || withTopic.Name != "Gaming" || withTopic.Topic != "All the games" {
		t.Fatalf("topic only: %+v, %v (name must stay)", withTopic, err)
	}
	if _, err := s.UpdateChannel(ctx, owner, c.ID, ptr("homework"), nil); !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("rename to taken name: %v", err)
	}
	if _, err := s.UpdateChannel(ctx, owner, c.ID, ptr("GAMING"), nil); err != nil {
		t.Errorf("changing only the case of its own name should work: %v", err)
	}
	if _, err := s.UpdateChannel(ctx, owner, 9999, ptr("X"), nil); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("unknown channel: %v", err)
	}
	if _, err := s.UpdateChannel(ctx, owner, c.ID, ptr(""), nil); validationField(err) != "name" {
		t.Errorf("empty name: %v", err)
	}
}

func TestDeleteChannelDeletesItsMessages(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "Games")
	keep := mustCreate(t, s, owner, "General")
	s.SendMessage(ctx, member, c.ID, "bye")
	s.SendMessage(ctx, member, keep.ID, "stays")

	if err := s.DeleteChannel(ctx, owner, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM messages").Scan(&n)
	if n != 1 {
		t.Errorf("messages left = %d, want 1 (only the other channel's)", n)
	}
	if err := s.DeleteChannel(ctx, owner, c.ID); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

// ---- messages ----

func TestSendMessage(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")

	m, err := s.SendMessage(ctx, member, c.ID, "hello\r\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "hello\nsecond line" {
		t.Errorf("content = %q, want Windows line ending normalized", m.Content)
	}
	if m.Author == nil || m.Author.ID != member.ID || m.Author.DisplayName != "Friend" || m.ChannelID != c.ID {
		t.Errorf("unexpected message %+v", m)
	}

	if _, err := s.SendMessage(ctx, member, c.ID, "مرحبا 👋\ttabs ok"); err != nil {
		t.Errorf("unicode, emoji and tabs should be allowed: %v", err)
	}
	if _, err := s.SendMessage(ctx, member, c.ID, strings.Repeat("😀", 4000)); err != nil {
		t.Errorf("4000 characters (emoji are 4 bytes each) should be allowed: %v", err)
	}
}

func TestSendMessageRules(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")

	for label, content := range map[string]string{
		"empty":        "",
		"only spaces":  " \n\t ",
		"too long":     strings.Repeat("a", 4001),
		"control char": "bell\x07",
		"null byte":    "nul\x00l",
		"bad utf-8":    "bad\xff",
	} {
		if _, err := s.SendMessage(ctx, member, c.ID, content); validationField(err) != "content" {
			t.Errorf("%s: err = %v, want content validation error", label, err)
		}
	}
	if _, err := s.SendMessage(ctx, member, 9999, "hi"); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestHistoryPagination(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	for i := 1; i <= 120; i++ {
		if _, err := s.SendMessage(ctx, member, c.ID, fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Newest page: messages 71-120, oldest first.
	page, more, err := s.ListMessages(ctx, c.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 50 || page[0].Content != "msg 71" || page[49].Content != "msg 120" || !more {
		t.Fatalf("page 1: %d msgs, first %q, last %q, more %v", len(page), page[0].Content, page[49].Content, more)
	}

	// Older: before the oldest one we have.
	page, more, _ = s.ListMessages(ctx, c.ID, page[0].ID, 50)
	if len(page) != 50 || page[0].Content != "msg 21" || page[49].Content != "msg 70" || !more {
		t.Fatalf("page 2: %d msgs, first %q, last %q, more %v", len(page), page[0].Content, page[49].Content, more)
	}

	page, more, _ = s.ListMessages(ctx, c.ID, page[0].ID, 50)
	if len(page) != 20 || page[0].Content != "msg 1" || more {
		t.Fatalf("page 3: %d msgs, first %q, more %v; want 20, msg 1, false", len(page), page[0].Content, more)
	}
}

func TestHistoryLimits(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	empty := mustCreate(t, s, owner, "Empty")
	for i := range 150 {
		s.SendMessage(ctx, member, c.ID, fmt.Sprint(i))
	}

	if page, _, _ := s.ListMessages(ctx, c.ID, 0, 500); len(page) != MaxPageSize {
		t.Errorf("limit 500 gave %d messages, want capped at %d", len(page), MaxPageSize)
	}
	if page, _, _ := s.ListMessages(ctx, c.ID, 0, 0); len(page) != DefaultPageSize {
		t.Errorf("limit 0 gave %d messages, want default %d", len(page), DefaultPageSize)
	}
	if page, more, err := s.ListMessages(ctx, empty.ID, 0, 50); err != nil || len(page) != 0 || more {
		t.Errorf("empty channel: %d msgs, more %v, err %v", len(page), more, err)
	}
	if _, _, err := s.ListMessages(ctx, 9999, 0, 50); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestHistoryDoesNotMixChannels(t *testing.T) {
	s, _, owner, member := setup(t)
	a := mustCreate(t, s, owner, "A")
	b := mustCreate(t, s, owner, "B")
	s.SendMessage(ctx, member, a.ID, "in A")
	s.SendMessage(ctx, member, b.ID, "in B")

	page, _, _ := s.ListMessages(ctx, a.ID, 0, 50)
	if len(page) != 1 || page[0].Content != "in A" {
		t.Errorf("channel A history = %+v", page)
	}
}

func TestRoomListPreview(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	s.SendMessage(ctx, owner, c.ID, "first")
	s.SendMessage(ctx, member, c.ID, strings.Repeat("ab", 80)) // 160 characters

	list, _ := s.ListChannels(ctx)
	p := list[0].LastMessage
	if p == nil || p.AuthorName != "Friend" {
		t.Fatalf("preview = %+v, want the newest message, by Friend", p)
	}
	if want := strings.Repeat("ab", 50) + "…"; p.Content != want {
		t.Errorf("preview content = %q, want the first 100 characters + …", p.Content)
	}
}

func TestMessagesOfDeletedAccountStay(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	s.SendMessage(ctx, member, c.ID, "I was here")

	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", member.ID); err != nil {
		t.Fatal(err)
	}

	page, _, err := s.ListMessages(ctx, c.ID, 0, 50)
	if err != nil || len(page) != 1 || page[0].Author != nil || page[0].Content != "I was here" {
		t.Errorf("history = %+v, err %v; want the message with no author", page, err)
	}
	list, _ := s.ListChannels(ctx)
	if p := list[0].LastMessage; p == nil || p.AuthorName != "" {
		t.Errorf("preview = %+v, want empty author name", p)
	}
}
