package api

import (
	"net/http"
	"sync"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
)

// fakeRealtime records what the API publishes.
type fakeRealtime struct {
	mu      sync.Mutex
	events  []string
	data    []any
	ended   []int64
	servedU []int64
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

	want := []string{"channel.created", "channel.updated", "channel.deleted"}
	if len(rt.events) != 3 || rt.events[0] != want[0] || rt.events[1] != want[1] || rt.events[2] != want[2] {
		t.Errorf("events %v, want %v", rt.events, want)
	}
	if d := rt.data[2].(map[string]int64); d["id"] != 3 {
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
