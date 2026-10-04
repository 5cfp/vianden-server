package api

import (
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/chat"
	"github.com/5cfp/vianden-server/internal/perm"
)

// fakeRealtime records what the API publishes.
type fakeRealtime struct {
	mu          sync.Mutex
	sentTo      []int64 // SendToUser targets (events are recorded in events/data too)
	events      []string
	data        []any
	ended       []int64
	servedU     []int64
	endedUsers  []int64
	filters     []func(perm.Role) bool // nil entry = sent to everyone (Broadcast)
	roleUpdates []perm.Role
}

func (f *fakeRealtime) Serve(w http.ResponseWriter, _ *http.Request, s accounts.Session) {
	f.mu.Lock()
	f.servedU = append(f.servedU, s.User.ID)
	f.mu.Unlock()
	w.WriteHeader(http.StatusSwitchingProtocols)
}

func (f *fakeRealtime) Broadcast(t string, d any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, t)
	f.data = append(f.data, d)
	f.filters = append(f.filters, nil)
}

func (f *fakeRealtime) EndSession(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, id)
}

func realtimeHandler(rt *fakeRealtime, c fakeChat) http.Handler {
	acc := fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
	return NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: memberToo{acc}, Chat: c, Realtime: rt, Logger: discardLogger})
}

func TestWebSocketNeedsLogin(t *testing.T) {
	rt := &fakeRealtime{}
	h := realtimeHandler(rt, fakeChat{})

	if rec := send(t, h, "GET", "/api/v1/ws", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", rec.Code)
	}
	if rec := send(t, h, "GET", "/api/v1/ws", "Bearer vs_wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d, want 401", rec.Code)
	}
	if len(rt.servedU) != 0 {
		t.Error("the hub was reached without a valid login")
	}
	send(t, h, "GET", "/api/v1/ws", "Bearer vs_member", "")
	if len(rt.servedU) != 1 || rt.servedU[0] != member.ID {
		t.Errorf("served users %v, want [member]", rt.servedU)
	}
}

func TestSendingAMessagePublishesIt(t *testing.T) {
	rt := &fakeRealtime{}
	send(t, realtimeHandler(rt, fakeChat{}), "POST", "/api/v1/channels/4/messages", "Bearer vs_member", `{"content":"live!"}`)

	if len(rt.events) != 1 || rt.events[0] != "message.created" {
		t.Fatalf("events %v, want [message.created]", rt.events)
	}
	m := rt.data[0].(messageResponse)
	if m.Content != "live!" || m.ChannelID != 4 || m.Author.ID != member.ID {
		t.Errorf("published %+v", m)
	}
}

func TestFailedActionsPublishNothing(t *testing.T) {
	rt := &fakeRealtime{}
	h := realtimeHandler(rt, fakeChat{err: accounts.ErrForbidden})
	send(t, h, "POST", "/api/v1/channels/4/messages", "Bearer vs_member", `{"content":"x"}`)
	send(t, h, "POST", "/api/v1/channels", "Bearer vs_member", `{"name":"x"}`)
	send(t, h, "DELETE", "/api/v1/channels/4", "Bearer vs_member", "")
	if len(rt.events) != 0 {
		t.Errorf("events %v, want none", rt.events)
	}
}

func TestChannelChangesArePublished(t *testing.T) {
	rt := &fakeRealtime{}
	h := realtimeHandler(rt, fakeChat{})
	send(t, h, "POST", "/api/v1/channels", "Bearer vs_owner", `{"name":"Games"}`)
	send(t, h, "PATCH", "/api/v1/channels/3", "Bearer vs_owner", `{"name":"Gaming"}`)
	send(t, h, "DELETE", "/api/v1/channels/3", "Bearer vs_owner", "")

	// An update also sends "channel.deleted" to users who LOST access; here nobody did.
	want := []string{"channel.created", "channel.updated", "channel.deleted", "channel.deleted"}
	if len(rt.events) != 4 || rt.events[0] != want[0] || rt.events[1] != want[1] || rt.events[2] != want[2] || rt.events[3] != want[3] {
		t.Errorf("events %v, want %v", rt.events, want)
	}
	if got := rt.audience(2); len(got) != 0 {
		t.Errorf("lost-access event reached %v, want nobody (the channel stayed visible to all)", got)
	}
	if d := rt.data[3].(map[string]int64); d["id"] != 3 {
		t.Errorf("channel.deleted data %v", d)
	}
}

func TestLogoutEndsLiveConnections(t *testing.T) {
	rt := &fakeRealtime{}
	send(t, realtimeHandler(rt, fakeChat{}), "POST", "/api/v1/logout", "Bearer vs_owner", "")
	if len(rt.ended) != 1 || rt.ended[0] != 1 {
		t.Errorf("ended sessions %v, want [1]", rt.ended)
	}
}

func (f *fakeRealtime) EndUser(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endedUsers = append(f.endedUsers, id)
}

func (f *fakeRealtime) UpdateUserRole(id int64, role perm.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roleUpdates = append(f.roleUpdates, role)
}

func (f *fakeRealtime) BroadcastWhere(t string, d any, to func(perm.Role) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, t)
	f.data = append(f.data, d)
	f.filters = append(f.filters, to)
}

// audience lists which roles would receive event i.
func (f *fakeRealtime) audience(i int) []perm.Role {
	var out []perm.Role
	for _, r := range perm.AllRoles {
		if f.filters[i] == nil || f.filters[i](r) {
			out = append(out, r)
		}
	}
	return out
}

func TestPrivateChannelEventsOnlyReachAllowedRoles(t *testing.T) {
	rt := &fakeRealtime{}
	h := realtimeHandler(rt, fakeChat{channelView: perm.Moderator})
	send(t, h, "POST", "/api/v1/channels/4/messages", "Bearer vs_owner", `{"content":"staff only"}`)

	want := []perm.Role{perm.Owner, perm.Admin, perm.Moderator}
	if got := rt.audience(0); !reflect.DeepEqual(got, want) {
		t.Errorf("message.created reached %v, want %v (never members)", got, want)
	}
}

func TestHidingAChannelTellsThoseWhoLostAccess(t *testing.T) {
	rt := &fakeRealtime{}
	h := realtimeHandler(rt, fakeChat{previousView: perm.Member})
	send(t, h, "PATCH", "/api/v1/channels/3", "Bearer vs_owner", `{"view_role":"admin"}`)

	if rt.events[0] != "channel.updated" || !reflect.DeepEqual(rt.audience(0), []perm.Role{perm.Owner, perm.Admin}) {
		t.Errorf("channel.updated reached %v", rt.audience(0))
	}
	if rt.events[1] != "channel.deleted" || !reflect.DeepEqual(rt.audience(1), []perm.Role{perm.Moderator, perm.Member}) {
		t.Errorf("channel.deleted (lost access) reached %v, want moderator and member", rt.audience(1))
	}
}

func TestReadOnlyError(t *testing.T) {
	rec := send(t, chatHandler(fakeChat{err: chat.ErrReadOnly}), "POST", "/api/v1/channels/1/messages", "Bearer vs_member", `{"content":"x"}`)
	if rec.Code != http.StatusForbidden || decode[errorResponse](t, rec).Error.Code != "read_only" {
		t.Errorf("status %d", rec.Code)
	}
}

func (f *fakeRealtime) SendToUser(id int64, t string, d any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentTo = append(f.sentTo, id)
	f.filters = append(f.filters, nil) // keeps filters[i] in step with events[i]
	f.events = append(f.events, t)
	f.data = append(f.data, d)
}

func (f *fakeRealtime) UpdateUserProfile(int64, string) {}
