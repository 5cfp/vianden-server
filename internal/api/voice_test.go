package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
	"github.com/5cfp/vianden-server/internal/voice"
)

type fakeVoice struct {
	err        error
	calls      *[]string
	userChange *[]int64
}

func (f fakeVoice) record(s string) {
	if f.calls != nil {
		*f.calls = append(*f.calls, s)
	}
}

func (fakeVoice) Available() bool { return true }
func (fakeVoice) List(context.Context, accounts.User) []voice.ChannelState {
	return []voice.ChannelState{{ChannelID: 5, Participants: []voice.ParticipantState{}}}
}
func (f fakeVoice) Disconnect(by accounts.User, ch, uid int64) error {
	f.record("disconnect")
	return f.err
}
func (f fakeVoice) SetServerMute(by accounts.User, ch, uid int64, muted bool) error {
	if muted {
		f.record("mute")
	} else {
		f.record("unmute")
	}
	return f.err
}
func (f fakeVoice) UserChanged(id int64) {
	if f.userChange != nil {
		*f.userChange = append(*f.userChange, id)
	}
}
func (f fakeVoice) ChannelChanged(context.Context, int64) { f.record("channel changed") }
func (f fakeVoice) ChannelDeleted(int64)                  { f.record("channel deleted") }

func voiceHandler(v fakeVoice, c fakeChat) http.Handler {
	acc := fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
	return NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: memberToo{acc}, Chat: c, Voice: v, Logger: discardLogger})
}

func TestInfoReportsVoice(t *testing.T) {
	rec := send(t, voiceHandler(fakeVoice{}, fakeChat{}), "GET", "/api/v1/info", "", "")
	if !strings.Contains(rec.Body.String(), `"voice":true`) {
		t.Errorf("info: %s", rec.Body)
	}
	// Without voice the flag is false.
	rec = send(t, chatHandler(fakeChat{}), "GET", "/api/v1/info", "", "")
	if !strings.Contains(rec.Body.String(), `"voice":false`) {
		t.Errorf("info without voice: %s", rec.Body)
	}
}

func TestListVoice(t *testing.T) {
	rec := send(t, voiceHandler(fakeVoice{}, fakeChat{}), "GET", "/api/v1/voice", "Bearer vs_member", "")
	want := `{"available":true,"channels":[{"channel_id":5,"participants":[]}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestVoiceModerationRoutes(t *testing.T) {
	var calls []string
	h := voiceHandler(fakeVoice{calls: &calls}, fakeChat{})
	if rec := send(t, h, "POST", "/api/v1/channels/5/voice/2/disconnect", "Bearer vs_owner", ""); rec.Code != http.StatusNoContent {
		t.Errorf("disconnect: %d %s", rec.Code, rec.Body)
	}
	if rec := send(t, h, "PUT", "/api/v1/channels/5/voice/2/mute", "Bearer vs_owner", `{"muted":true}`); rec.Code != http.StatusNoContent {
		t.Errorf("mute: %d", rec.Code)
	}
	send(t, h, "PUT", "/api/v1/channels/5/voice/2/mute", "Bearer vs_owner", `{"muted":false}`)
	if strings.Join(calls, ",") != "disconnect,mute,unmute" {
		t.Errorf("calls %v", calls)
	}

	for _, c := range []struct {
		err  error
		code int
		want string
	}{
		{accounts.ErrForbidden, http.StatusForbidden, "forbidden"},
		{voice.ErrNotInVoice, http.StatusNotFound, "not_in_voice"},
		{voice.ErrUnavailable, http.StatusServiceUnavailable, "voice_unavailable"},
	} {
		rec := send(t, voiceHandler(fakeVoice{err: c.err}, fakeChat{}), "POST", "/api/v1/channels/5/voice/2/disconnect", "Bearer vs_member", "")
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
	// A channel you cannot see: 404 before voice is even asked.
	var none []string
	rec := send(t, voiceHandler(fakeVoice{calls: &none}, fakeChat{err: chat404}), "POST", "/api/v1/channels/5/voice/2/disconnect", "Bearer vs_owner", "")
	if rec.Code != http.StatusNotFound || len(none) != 0 {
		t.Errorf("hidden channel: %d, calls %v", rec.Code, none)
	}
}

func TestVoiceHearsAboutChanges(t *testing.T) {
	var calls []string
	var users []int64
	rt := &fakeRealtime{}
	acc := fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
	h := NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: memberToo{acc}, Chat: fakeChat{},
		Voice: fakeVoice{calls: &calls, userChange: &users}, Realtime: rt, Logger: discardLogger})

	send(t, h, "PATCH", "/api/v1/channels/5", "Bearer vs_owner", `{"name":"x"}`)
	send(t, h, "DELETE", "/api/v1/channels/5", "Bearer vs_owner", "")
	if strings.Join(calls, ",") != "channel changed,channel deleted" {
		t.Errorf("voice calls %v", calls)
	}
	// A role change reaches voice through the realtime wrapper.
	withVoice{rt, fakeVoice{userChange: &users}}.UpdateUserRole(9, perm.Moderator)
	if len(users) != 1 || users[0] != 9 {
		t.Errorf("voice user changes %v", users)
	}
	// The type of a channel cannot be changed.
	if rec := send(t, h, "PATCH", "/api/v1/channels/5", "Bearer vs_owner", `{"type":"voice"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("type change: %d", rec.Code)
	}
}
