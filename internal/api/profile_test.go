package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/files"
)

type fakeProfiles struct {
	member accounts.Member
	err    error
}

func (f fakeProfiles) UpdateDisplayName(_ context.Context, id int64, name string) (accounts.Member, error) {
	m := f.member
	m.ID, m.DisplayName = id, name
	return m, f.err
}

func (f fakeProfiles) GetMember(_ context.Context, id int64) (accounts.Member, error) {
	m := f.member
	m.ID = id
	return m, f.err
}

type fakeAvatars struct {
	got *string
	err error
}

func (f fakeAvatars) SetAvatar(_ context.Context, _ int64, body io.Reader) error {
	b, _ := io.ReadAll(body)
	if f.got != nil {
		*f.got = string(b)
	}
	return f.err
}
func (f fakeAvatars) RemoveAvatar(context.Context, int64) error { return f.err }
func (f fakeAvatars) OpenAvatar(key string) (io.ReadSeekCloser, error) {
	if key != strings.Repeat("a", 64) {
		return nil, files.ErrNotFound
	}
	return nopCloser{strings.NewReader("PNG")}, nil
}

func profileHandler(rt Realtime, p fakeProfiles, av fakeAvatars) http.Handler {
	acc := fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
	return NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: memberToo{acc}, Chat: fakeChat{},
		Profiles: p, Avatars: av, Realtime: rt, Logger: discardLogger})
}

func TestUpdateDisplayName(t *testing.T) {
	rt := &fakeRealtime{}
	h := profileHandler(rt, fakeProfiles{member: accounts.Member{User: member}}, fakeAvatars{})
	rec := send(t, h, "PATCH", "/api/v1/me", "Bearer vs_member", `{"display_name":"New Name"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"display_name":"New Name"`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(rt.events) != 1 || rt.events[0] != "member.updated" {
		t.Errorf("events %v, want member.updated", rt.events)
	}
	// It is always YOUR profile: there is no user id in the request to change.
	if rec := send(t, h, "PATCH", "/api/v1/me", "Bearer vs_member", `{"display_name":"x","id":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("extra id field: %d", rec.Code)
	}
	bad := profileHandler(rt, fakeProfiles{err: &accounts.ValidationError{Field: "display_name", Message: "x"}}, fakeAvatars{})
	if rec := send(t, bad, "PATCH", "/api/v1/me", "Bearer vs_member", `{"display_name":""}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "invalid_display_name") {
		t.Errorf("invalid name: %d %s", rec.Code, rec.Body)
	}
}

func TestAvatarEndpoints(t *testing.T) {
	var got string
	rt := &fakeRealtime{}
	withAvatar := accounts.Member{User: accounts.User{Username: "friend", DisplayName: "Friend", AvatarKey: strings.Repeat("a", 64)}}
	h := profileHandler(rt, fakeProfiles{member: withAvatar}, fakeAvatars{got: &got})

	rec := send(t, h, "PUT", "/api/v1/me/avatar", "Bearer vs_member", "IMAGEBYTES")
	if rec.Code != http.StatusOK || got != "IMAGEBYTES" {
		t.Fatalf("status %d, got %q", rec.Code, got)
	}
	if !strings.Contains(rec.Body.String(), `"avatar":"/api/v1/avatars/`+strings.Repeat("a", 64)+`"`) {
		t.Errorf("response %s", rec.Body)
	}
	if rt.events[0] != "member.updated" {
		t.Errorf("events %v", rt.events)
	}

	img := send(t, h, "GET", "/api/v1/avatars/"+strings.Repeat("a", 64), "Bearer vs_member", "")
	if img.Code != http.StatusOK || img.Header().Get("Content-Type") != "image/png" || img.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("get avatar: %d %v", img.Code, img.Header())
	}
	if rec := send(t, h, "GET", "/api/v1/avatars/nope", "Bearer vs_member", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown avatar: %d", rec.Code)
	}
	if rec := send(t, h, "DELETE", "/api/v1/me/avatar", "Bearer vs_member", ""); rec.Code != http.StatusOK {
		t.Errorf("remove: %d", rec.Code)
	}

	big := profileHandler(rt, fakeProfiles{}, fakeAvatars{err: files.ErrTooLarge})
	if rec := send(t, big, "PUT", "/api/v1/me/avatar", "Bearer vs_member", "x"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
	notImage := profileHandler(rt, fakeProfiles{}, fakeAvatars{err: &accounts.ValidationError{Field: "avatar", Message: "x"}})
	if rec := send(t, notImage, "PUT", "/api/v1/me/avatar", "Bearer vs_member", "x"); !strings.Contains(rec.Body.String(), "invalid_avatar") {
		t.Errorf("not an image: %s", rec.Body)
	}
}
