package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
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
	role := perm.Member
	if owner {
		role = perm.Owner
	}
	u := accounts.User{Username: username, DisplayName: display, Role: role}
	err := pool.QueryRow(ctx, "INSERT INTO users (username, display_name, password_hash, role) VALUES ($1, $2, 'x', $3) RETURNING id",
		username, display, string(role)).Scan(&u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustCreate(t *testing.T, s *Service, owner accounts.User, name string) Channel {
	t.Helper()
	c, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: name, Topic: ""})
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

	a, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: "  Games ", Topic: " fun stuff "})
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

	list, err := s.ListChannels(ctx, owner)
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

	if _, err := s.CreateChannel(ctx, member, ChannelSettings{Name: "Mine", Topic: ""}); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("member create: %v, want ErrForbidden", err)
	}
	if _, _, err := s.UpdateChannel(ctx, member, c.ID, ChannelChanges{Name: ptr("Hacked")}); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("member rename: %v, want ErrForbidden", err)
	}
	if _, err := s.DeleteChannel(ctx, member, c.ID); !errors.Is(err, accounts.ErrForbidden) {
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
		if _, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: c.name, Topic: c.topic}); validationField(err) != c.field {
			t.Errorf("%s: err = %v, want %s validation error", label, err, c.field)
		}
	}
	if _, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: "GAMES", Topic: ""}); !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("duplicate name (other case): %v, want ErrChannelNameTaken", err)
	}
	if _, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Spiele 🎮 العاب", Topic: ""}); err != nil {
		t.Errorf("any language and emoji should be allowed: %v", err)
	}
	if _, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: strings.Repeat("é", 32)}); err != nil {
		t.Errorf("32 characters (64 bytes) should be allowed: %v", err)
	}
}

func TestUpdateChannel(t *testing.T) {
	s, _, owner, _ := setup(t)
	c := mustCreate(t, s, owner, "Games")
	mustCreate(t, s, owner, "Homework")

	renamed, _, err := s.UpdateChannel(ctx, owner, c.ID, ChannelChanges{Name: ptr("Gaming")})
	if err != nil || renamed.Name != "Gaming" || renamed.Topic != "" {
		t.Fatalf("rename: %+v, %v", renamed, err)
	}
	withTopic, _, err := s.UpdateChannel(ctx, owner, c.ID, ChannelChanges{Topic: ptr("All the games")})
	if err != nil || withTopic.Name != "Gaming" || withTopic.Topic != "All the games" {
		t.Fatalf("topic only: %+v, %v (name must stay)", withTopic, err)
	}
	if _, _, err := s.UpdateChannel(ctx, owner, c.ID, ChannelChanges{Name: ptr("homework")}); !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("rename to taken name: %v", err)
	}
	if _, _, err := s.UpdateChannel(ctx, owner, c.ID, ChannelChanges{Name: ptr("GAMING")}); err != nil {
		t.Errorf("changing only the case of its own name should work: %v", err)
	}
	if _, _, err := s.UpdateChannel(ctx, owner, 9999, ChannelChanges{Name: ptr("X")}); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("unknown channel: %v", err)
	}
	if _, _, err := s.UpdateChannel(ctx, owner, c.ID, ChannelChanges{Name: ptr("")}); validationField(err) != "name" {
		t.Errorf("empty name: %v", err)
	}
}

func TestDeleteChannelDeletesItsMessages(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "Games")
	keep := mustCreate(t, s, owner, "General")
	s.SendMessage(ctx, member, c.ID, "bye")
	s.SendMessage(ctx, member, keep.ID, "stays")

	if _, err := s.DeleteChannel(ctx, owner, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM messages").Scan(&n)
	if n != 1 {
		t.Errorf("messages left = %d, want 1 (only the other channel's)", n)
	}
	if _, err := s.DeleteChannel(ctx, owner, c.ID); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

// ---- messages ----

func TestSendMessage(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")

	m, _, err := s.SendMessage(ctx, member, c.ID, "hello\r\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "hello\nsecond line" {
		t.Errorf("content = %q, want Windows line ending normalized", m.Content)
	}
	if m.Author == nil || m.Author.ID != member.ID || m.Author.DisplayName != "Friend" || m.ChannelID != c.ID {
		t.Errorf("unexpected message %+v", m)
	}

	if _, _, err := s.SendMessage(ctx, member, c.ID, "مرحبا 👋\ttabs ok"); err != nil {
		t.Errorf("unicode, emoji and tabs should be allowed: %v", err)
	}
	if _, _, err := s.SendMessage(ctx, member, c.ID, strings.Repeat("😀", 4000)); err != nil {
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
		if _, _, err := s.SendMessage(ctx, member, c.ID, content); validationField(err) != "content" {
			t.Errorf("%s: err = %v, want content validation error", label, err)
		}
	}
	if _, _, err := s.SendMessage(ctx, member, 9999, "hi"); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestHistoryPagination(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	for i := 1; i <= 120; i++ {
		if _, _, err := s.SendMessage(ctx, member, c.ID, fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Newest page: messages 71-120, oldest first.
	page, more, err := s.ListMessages(ctx, owner, c.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 50 || page[0].Content != "msg 71" || page[49].Content != "msg 120" || !more {
		t.Fatalf("page 1: %d msgs, first %q, last %q, more %v", len(page), page[0].Content, page[49].Content, more)
	}

	// Older: before the oldest one we have.
	page, more, _ = s.ListMessages(ctx, owner, c.ID, page[0].ID, 50)
	if len(page) != 50 || page[0].Content != "msg 21" || page[49].Content != "msg 70" || !more {
		t.Fatalf("page 2: %d msgs, first %q, last %q, more %v", len(page), page[0].Content, page[49].Content, more)
	}

	page, more, _ = s.ListMessages(ctx, owner, c.ID, page[0].ID, 50)
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

	if page, _, _ := s.ListMessages(ctx, owner, c.ID, 0, 500); len(page) != MaxPageSize {
		t.Errorf("limit 500 gave %d messages, want capped at %d", len(page), MaxPageSize)
	}
	if page, _, _ := s.ListMessages(ctx, owner, c.ID, 0, 0); len(page) != DefaultPageSize {
		t.Errorf("limit 0 gave %d messages, want default %d", len(page), DefaultPageSize)
	}
	if page, more, err := s.ListMessages(ctx, owner, empty.ID, 0, 50); err != nil || len(page) != 0 || more {
		t.Errorf("empty channel: %d msgs, more %v, err %v", len(page), more, err)
	}
	if _, _, err := s.ListMessages(ctx, owner, 9999, 0, 50); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestHistoryDoesNotMixChannels(t *testing.T) {
	s, _, owner, member := setup(t)
	a := mustCreate(t, s, owner, "A")
	b := mustCreate(t, s, owner, "B")
	s.SendMessage(ctx, member, a.ID, "in A")
	s.SendMessage(ctx, member, b.ID, "in B")

	page, _, _ := s.ListMessages(ctx, owner, a.ID, 0, 50)
	if len(page) != 1 || page[0].Content != "in A" {
		t.Errorf("channel A history = %+v", page)
	}
}

func TestRoomListPreview(t *testing.T) {
	s, _, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	s.SendMessage(ctx, owner, c.ID, "first")
	s.SendMessage(ctx, member, c.ID, strings.Repeat("ab", 80)) // 160 characters

	list, _ := s.ListChannels(ctx, owner)
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

	page, _, err := s.ListMessages(ctx, owner, c.ID, 0, 50)
	if err != nil || len(page) != 1 || page[0].Author != nil || page[0].Content != "I was here" {
		t.Errorf("history = %+v, err %v; want the message with no author", page, err)
	}
	list, _ := s.ListChannels(ctx, owner)
	if p := list[0].LastMessage; p == nil || p.AuthorName != "" {
		t.Errorf("preview = %+v, want empty author name", p)
	}
}

func TestChannelManagementFollowsRoles(t *testing.T) {
	s, pool, _, _ := setup(t)
	for role, allowed := range map[perm.Role]bool{perm.Admin: true, perm.Moderator: false, perm.Member: false} {
		u := addUser(t, pool, "user_"+string(role), "U", false)
		u.Role = role
		_, err := s.CreateChannel(ctx, u, ChannelSettings{Name: "Room of " + string(role), Topic: ""})
		if allowed && err != nil {
			t.Errorf("%s should manage channels: %v", role, err)
		}
		if !allowed && !errors.Is(err, accounts.ErrForbidden) {
			t.Errorf("%s: err = %v, want ErrForbidden", role, err)
		}
	}
}

// ---- channel access (M5) ----

func withRole(u accounts.User, r perm.Role) accounts.User {
	u.Role = r
	return u
}

func names(cs []Channel) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestPrivateChannelIsInvisibleToLowerRoles(t *testing.T) {
	s, _, owner, member := setup(t)
	mustCreate(t, s, owner, "General")
	staff, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Staff", ViewRole: perm.Moderator})
	if err != nil {
		t.Fatal(err)
	}
	mod := withRole(member, perm.Moderator)
	s.SendMessage(ctx, mod, staff.ID, "secret plans")

	list, _ := s.ListChannels(ctx, member)
	if got := names(list); len(got) != 1 || got[0] != "General" {
		t.Errorf("member sees %v, want only General", got)
	}
	list, _ = s.ListChannels(ctx, mod)
	if len(list) != 2 {
		t.Errorf("moderator sees %v, want General and Staff", names(list))
	}

	// For the member, the private channel behaves as if it did not exist (no "forbidden").
	if _, _, err := s.ListMessages(ctx, member, staff.ID, 0, 50); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("member reads private history: %v, want ErrChannelNotFound", err)
	}
	if _, _, err := s.SendMessage(ctx, member, staff.ID, "let me in"); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("member writes in private channel: %v, want ErrChannelNotFound", err)
	}
	if page, _, err := s.ListMessages(ctx, mod, staff.ID, 0, 50); err != nil || len(page) != 1 {
		t.Errorf("moderator history: %d msgs, %v", len(page), err)
	}
}

func TestReadOnlyChannel(t *testing.T) {
	s, _, owner, member := setup(t)
	news, _ := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Announcements", SendRole: perm.Moderator})
	if news.ViewRole != perm.Member || news.SendRole != perm.Moderator {
		t.Fatalf("roles %s/%s, want member/moderator", news.ViewRole, news.SendRole)
	}

	if _, _, err := s.SendMessage(ctx, member, news.ID, "hi"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("member writes in read-only channel: %v, want ErrReadOnly", err)
	}
	if _, _, err := s.ListMessages(ctx, member, news.ID, 0, 50); err != nil {
		t.Errorf("member must still read it: %v", err)
	}
	if _, _, err := s.SendMessage(ctx, withRole(member, perm.Moderator), news.ID, "news!"); err != nil {
		t.Errorf("moderator writes: %v", err)
	}
}

func TestChannelAccessSettingsRules(t *testing.T) {
	s, _, owner, member := setup(t)
	admin := withRole(member, perm.Admin)

	var ve *accounts.ValidationError
	if _, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Weird", ViewRole: perm.Admin, SendRole: perm.Member}); !errors.As(err, &ve) || ve.Field != "send_role" {
		t.Errorf("send below view: %v, want send_role ValidationError", err)
	}
	if _, err := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Bad", ViewRole: "root"}); !errors.As(err, &ve) || ve.Field != "view_role" {
		t.Errorf("invalid role: %v", err)
	}
	if c, _ := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Mods", ViewRole: perm.Moderator}); c.SendRole != perm.Moderator {
		t.Errorf("send_role defaults to view_role, got %s", c.SendRole)
	}

	// Escalation guard: an admin cannot create or raise a channel above their own role.
	if _, err := s.CreateChannel(ctx, admin, ChannelSettings{Name: "Mine", ViewRole: perm.Owner}); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("admin creates owner-only channel: %v, want ErrForbidden", err)
	}
	ownerOnly, _ := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Owner only", ViewRole: perm.Owner})
	toAdmin := perm.Admin
	if _, _, err := s.UpdateChannel(ctx, admin, ownerOnly.ID, ChannelChanges{ViewRole: &toAdmin}); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("admin un-hides owner-only channel: %v, want ErrChannelNotFound", err)
	}
	if _, err := s.DeleteChannel(ctx, admin, ownerOnly.ID); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("admin deletes owner-only channel: %v, want ErrChannelNotFound", err)
	}
	if _, _, err := s.UpdateChannel(ctx, owner, ownerOnly.ID, ChannelChanges{ViewRole: &toAdmin}); err != nil {
		t.Errorf("owner opens it to admins: %v", err)
	}
}

func TestUpdateReportsPreviousViewRole(t *testing.T) {
	s, _, owner, _ := setup(t)
	c := mustCreate(t, s, owner, "General")
	mods := perm.Moderator
	updated, previous, err := s.UpdateChannel(ctx, owner, c.ID, ChannelChanges{ViewRole: &mods})
	if updated.SendRole != perm.Moderator {
		t.Errorf("send role %s: should rise with the view role", updated.SendRole)
	}
	if err != nil || previous != perm.Member || updated.ViewRole != perm.Moderator {
		t.Errorf("updated %s, previous %s, err %v", updated.ViewRole, previous, err)
	}
}

func TestChannelAccessLookup(t *testing.T) {
	s, _, owner, _ := setup(t)
	c, _ := s.CreateChannel(ctx, owner, ChannelSettings{Name: "News", SendRole: perm.Admin})
	view, send, ok := s.ChannelAccess(ctx, c.ID)
	if !ok || view != perm.Member || send != perm.Admin {
		t.Errorf("ChannelAccess = %s, %s, %v", view, send, ok)
	}
	if _, _, ok := s.ChannelAccess(ctx, 9999); ok {
		t.Error("unknown channel reported as existing")
	}
}

// ---- message deletion (M5) ----

func TestModeratorDeletesMessage(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	m, _, _ := s.SendMessage(ctx, member, c.ID, "something rude")
	s.SendMessage(ctx, member, c.ID, "something nice")
	mod := withRole(addUser(t, pool, "mod", "Mod", false), perm.Moderator)
	pool.Exec(ctx, "UPDATE users SET role = 'moderator' WHERE id = $1", mod.ID)

	if _, err := s.DeleteMessage(ctx, mod, c.ID, m.ID); err != nil {
		t.Fatal(err)
	}

	// The text is really gone from the database, not just hidden.
	var content string
	var deletedBy int64
	pool.QueryRow(ctx, "SELECT content, deleted_by FROM messages WHERE id = $1", m.ID).Scan(&content, &deletedBy)
	if content != "" || deletedBy != mod.ID {
		t.Errorf("stored content %q, deleted_by %d; want erased, by the moderator", content, deletedBy)
	}

	page, _, _ := s.ListMessages(ctx, member, c.ID, 0, 50)
	if len(page) != 2 || !page[0].Deleted || page[0].Content != "" || page[0].Author == nil || page[1].Deleted {
		t.Errorf("history %+v: want a placeholder (author kept) then the nice message", page)
	}
	list, _ := s.ListChannels(ctx, member)
	if p := list[0].LastMessage; p == nil || p.Content != "something nice" {
		t.Errorf("preview %+v: should skip deleted messages", p)
	}

	if _, err := s.DeleteMessage(ctx, mod, c.ID, m.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("deleting twice: %v, want ErrMessageNotFound", err)
	}
}

func TestMessageDeletionRules(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	staff, _ := s.CreateChannel(ctx, owner, ChannelSettings{Name: "Staff", ViewRole: perm.Admin})
	byMember, _, _ := s.SendMessage(ctx, member, c.ID, "member text")
	byOwner, _, _ := s.SendMessage(ctx, owner, c.ID, "owner text")
	inStaff, _, _ := s.SendMessage(ctx, owner, staff.ID, "admin talk")

	mod := withRole(member, perm.Moderator)
	other := withRole(addUser(t, pool, "other", "Other", false), perm.Member)

	if _, err := s.DeleteMessage(ctx, other, c.ID, byMember.ID); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("member deletes someone's message: %v, want ErrForbidden (own messages come in M6)", err)
	}
	if _, err := s.DeleteMessage(ctx, mod, c.ID, byOwner.ID); !errors.Is(err, accounts.ErrForbidden) {
		t.Errorf("moderator deletes the owner's message: %v, want ErrForbidden", err)
	}
	if _, err := s.DeleteMessage(ctx, mod, staff.ID, inStaff.ID); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("moderator deletes in a channel they cannot see: %v, want ErrChannelNotFound", err)
	}
	if _, err := s.DeleteMessage(ctx, owner, staff.ID, byMember.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("message id from another channel: %v, want ErrMessageNotFound", err)
	}
	if _, err := s.DeleteMessage(ctx, owner, c.ID, byMember.ID); err != nil {
		t.Errorf("owner deletes a member's message: %v", err)
	}
}

func TestDeletedAccountMessagesCanBeRemoved(t *testing.T) {
	s, pool, owner, member := setup(t)
	c := mustCreate(t, s, owner, "General")
	m, _, _ := s.SendMessage(ctx, member, c.ID, "old message")
	pool.Exec(ctx, "DELETE FROM users WHERE id = $1", member.ID)

	mod := withRole(addUser(t, pool, "mod", "Mod", false), perm.Moderator)
	if _, err := s.DeleteMessage(ctx, mod, c.ID, m.ID); err != nil {
		t.Errorf("deleting a message of a deleted account: %v", err)
	}
}

func TestDatabaseRejectsHalfDeletedMessages(t *testing.T) {
	s, pool, owner, _ := setup(t)
	c := mustCreate(t, s, owner, "General")
	m, _, _ := s.SendMessage(ctx, owner, c.ID, "keep me consistent")
	// "deleted" but text still there: the database refuses (defense in depth).
	if _, err := pool.Exec(ctx, "UPDATE messages SET deleted_at = now() WHERE id = $1", m.ID); err == nil {
		t.Error("a deleted message that still has its text was accepted")
	}
}
