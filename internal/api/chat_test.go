package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/chat"
)

// fakeChat returns fixed results and records what it was asked.
type fakeChat struct {
	channels []chat.Channel
	messages []chat.Message
	hasMore  bool
	err      error
	got      *chatCall
}

type chatCall struct {
	by          accounts.User
	channelID   int64
	name, topic *string
	content     string
	before      int64
	limit       int
}

func (f fakeChat) record(c chatCall) {
	if f.got != nil {
		*f.got = c
	}
}

func (f fakeChat) ListChannels(context.Context) ([]chat.Channel, error) { return f.channels, f.err }

func (f fakeChat) CreateChannel(_ context.Context, by accounts.User, name, topic string) (chat.Channel, error) {
	f.record(chatCall{by: by, name: &name, topic: &topic})
	if f.err != nil {
		return chat.Channel{}, f.err
	}
	return chat.Channel{ID: 3, Name: name, Topic: topic, Type: "text", Position: 2}, nil
}

func (f fakeChat) UpdateChannel(_ context.Context, by accounts.User, id int64, name, topic *string) (chat.Channel, error) {
	f.record(chatCall{by: by, channelID: id, name: name, topic: topic})
	if f.err != nil {
		return chat.Channel{}, f.err
	}
	return chat.Channel{ID: id, Name: "Renamed", Type: "text"}, nil
}

func (f fakeChat) DeleteChannel(_ context.Context, by accounts.User, id int64) error {
	f.record(chatCall{by: by, channelID: id})
	return f.err
}

func (f fakeChat) SendMessage(_ context.Context, by accounts.User, channelID int64, content string) (chat.Message, error) {
	f.record(chatCall{by: by, channelID: channelID, content: content})
	if f.err != nil {
		return chat.Message{}, f.err
	}
	return chat.Message{ID: 10, ChannelID: channelID, Content: content, CreatedAt: when,
		Author: &chat.Author{ID: by.ID, Username: by.Username, DisplayName: by.DisplayName}}, nil
}

func (f fakeChat) ListMessages(_ context.Context, channelID, before int64, limit int) ([]chat.Message, bool, error) {
	f.record(chatCall{channelID: channelID, before: before, limit: limit})
	return f.messages, f.hasMore, f.err
}

var when = time.Date(2026, 10, 4, 18, 30, 0, 0, time.UTC)

var member = accounts.User{ID: 2, Username: "friend", DisplayName: "Friend"}

// chatHandler: a handler where "vs_owner" is the owner and "vs_member" a member.
func chatHandler(c fakeChat) http.Handler {
	acc := fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
	return NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: memberToo{acc}, Chat: c, Logger: discardLogger})
}

// memberToo accepts both the owner's and the member's token.
type memberToo struct{ fakeAccounts }

func (m memberToo) Authenticate(ctx context.Context, token string) (accounts.Session, error) {
	if token == "vs_member" {
		return accounts.Session{ID: 2, User: member}, nil
	}
	return m.fakeAccounts.Authenticate(ctx, token)
}

func TestChatEndpointsNeedLogin(t *testing.T) {
	h := chatHandler(fakeChat{})
	for _, r := range [][2]string{
		{"GET", "/api/v1/channels"}, {"POST", "/api/v1/channels"}, {"PATCH", "/api/v1/channels/1"},
		{"DELETE", "/api/v1/channels/1"}, {"GET", "/api/v1/channels/1/messages"}, {"POST", "/api/v1/channels/1/messages"},
	} {
		if rec := send(t, h, r[0], r[1], "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d, want 401", r[0], r[1], rec.Code)
		}
	}
}

func TestListChannelsResponse(t *testing.T) {
	h := chatHandler(fakeChat{channels: []chat.Channel{
		{ID: 1, Name: "General", Topic: "chat", Type: "text", LastMessage: &chat.Preview{AuthorName: "Sara", Content: "hi", CreatedAt: when}},
		{ID: 2, Name: "Empty", Type: "text", Position: 1},
	}})
	rec := send(t, h, "GET", "/api/v1/channels", "Bearer vs_member", "")
	want := `{"channels":[` +
		`{"id":1,"name":"General","topic":"chat","type":"text","position":0,"last_message":{"author_name":"Sara","content":"hi","created_at":"2026-10-04T18:30:00Z"}},` +
		`{"id":2,"name":"Empty","topic":"","type":"text","position":1,"last_message":null}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("status %d\n got: %s\nwant: %s", rec.Code, rec.Body, want)
	}
}

func TestCreateChannelPassesUserAndFields(t *testing.T) {
	var got chatCall
	rec := send(t, chatHandler(fakeChat{got: &got}), "POST", "/api/v1/channels", "Bearer vs_owner", `{"name":"Games","topic":"fun"}`)
	if rec.Code != http.StatusCreated || got.by.ID != osama.ID || *got.name != "Games" || *got.topic != "fun" {
		t.Errorf("status %d, call %+v", rec.Code, got)
	}
}

func TestUpdateChannelSendsOnlyGivenFields(t *testing.T) {
	var got chatCall
	rec := send(t, chatHandler(fakeChat{got: &got}), "PATCH", "/api/v1/channels/7", "Bearer vs_owner", `{"topic":"new topic"}`)
	if rec.Code != http.StatusOK || got.channelID != 7 || got.name != nil || got.topic == nil || *got.topic != "new topic" {
		t.Errorf("status %d, call %+v (name must be nil = unchanged)", rec.Code, got)
	}
}

func TestChannelErrorMapping(t *testing.T) {
	cases := []struct {
		err      error
		wantCode int
		wantErr  string
	}{
		{accounts.ErrForbidden, 403, "forbidden"},
		{chat.ErrChannelNotFound, 404, "not_found"},
		{chat.ErrChannelNameTaken, 409, "channel_name_taken"},
		{&accounts.ValidationError{Field: "name", Message: "bad"}, 400, "invalid_name"},
		{errors.New("db exploded"), 500, "internal_error"},
	}
	for _, c := range cases {
		rec := send(t, chatHandler(fakeChat{err: c.err}), "POST", "/api/v1/channels", "Bearer vs_owner", `{"name":"X"}`)
		if rec.Code != c.wantCode || decode[errorResponse](t, rec).Error.Code != c.wantErr {
			t.Errorf("%v: got %d, want %d %s", c.err, rec.Code, c.wantCode, c.wantErr)
		}
	}
}

func TestBadChannelIDs(t *testing.T) {
	h := chatHandler(fakeChat{})
	for _, id := range []string{"abc", "0", "-1", "99999999999999999999"} {
		for _, r := range [][2]string{{"PATCH", "/api/v1/channels/"}, {"DELETE", "/api/v1/channels/"}, {"GET", "/api/v1/channels/%s/messages"}} {
			path := r[1] + id
			if strings.Contains(r[1], "%s") {
				path = fmt.Sprintf(r[1], id)
			}
			if rec := send(t, h, r[0], path, "Bearer vs_owner", `{"name":"x"}`); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", r[0], path, rec.Code)
			}
		}
	}
}

func TestSendMessage(t *testing.T) {
	var got chatCall
	rec := send(t, chatHandler(fakeChat{got: &got}), "POST", "/api/v1/channels/4/messages", "Bearer vs_member", `{"content":"hello"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got.by.ID != member.ID || got.channelID != 4 || got.content != "hello" {
		t.Errorf("call %+v: the message must be sent as the logged-in user", got)
	}
	want := `{"message":{"id":10,"channel_id":4,"author":{"id":2,"username":"friend","display_name":"Friend"},"content":"hello","created_at":"2026-10-04T18:30:00Z"}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("\n got: %s\nwant: %s", rec.Body, want)
	}
}

func TestCannotSendAsSomeoneElse(t *testing.T) {
	// Extra fields such as "author_id" are rejected outright: the author is always the session's user.
	rec := send(t, chatHandler(fakeChat{}), "POST", "/api/v1/channels/4/messages", "Bearer vs_member", `{"content":"hi","author_id":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

func TestSendingIsRateLimitedPerUser(t *testing.T) {
	h := chatHandler(fakeChat{})
	for i := range 10 {
		if rec := send(t, h, "POST", "/api/v1/channels/1/messages", "Bearer vs_member", `{"content":"spam"}`); rec.Code != http.StatusCreated {
			t.Fatalf("message %d: %d", i+1, rec.Code)
		}
	}
	rec := send(t, h, "POST", "/api/v1/channels/1/messages", "Bearer vs_member", `{"content":"spam"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("11th message: %d, want 429 with Retry-After", rec.Code)
	}
	// Another user (same IP in this test) is not affected.
	if rec := send(t, h, "POST", "/api/v1/channels/1/messages", "Bearer vs_owner", `{"content":"hi"}`); rec.Code != http.StatusCreated {
		t.Errorf("other user blocked: %d", rec.Code)
	}
}

func TestListMessagesQuery(t *testing.T) {
	var got chatCall
	c := fakeChat{got: &got, hasMore: true, messages: []chat.Message{{ID: 5, ChannelID: 1, Content: "old", CreatedAt: when}}}

	rec := send(t, chatHandler(c), "GET", "/api/v1/channels/1/messages?before=42&limit=20", "Bearer vs_member", "")
	if rec.Code != http.StatusOK || got.before != 42 || got.limit != 20 {
		t.Fatalf("status %d, call %+v", rec.Code, got)
	}
	want := `{"messages":[{"id":5,"channel_id":1,"author":null,"content":"old","created_at":"2026-10-04T18:30:00Z"}],"has_more":true}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("\n got: %s\nwant: %s", rec.Body, want)
	}

	send(t, chatHandler(c), "GET", "/api/v1/channels/1/messages", "Bearer vs_member", "")
	if got.before != 0 || got.limit != 0 {
		t.Errorf("defaults: before %d, limit %d; want 0, 0 (service picks the default)", got.before, got.limit)
	}

	for _, q := range []string{"before=abc", "before=0", "before=-5", "limit=0", "limit=101", "limit=x"} {
		if rec := send(t, chatHandler(c), "GET", "/api/v1/channels/1/messages?"+q, "Bearer vs_member", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("?%s: %d, want 400", q, rec.Code)
		}
	}
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	h := chatHandler(fakeChat{})
	if rec := send(t, h, "GET", "/api/v1/channels", "Bearer vs_member", ""); rec.Body.String() != "{\"channels\":[]}\n" {
		t.Errorf("channels: %s", rec.Body)
	}
	if rec := send(t, h, "GET", "/api/v1/channels/1/messages", "Bearer vs_member", ""); rec.Body.String() != "{\"messages\":[],\"has_more\":false}\n" {
		t.Errorf("messages: %s", rec.Body)
	}
}
