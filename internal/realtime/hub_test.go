package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
)

// testServer runs the hub behind a tiny HTTP server. The "token" is just "<userID>-<sessionID>".
func testServer(t *testing.T) (*Hub, string) {
	t.Helper()
	hub := NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")
		uid, _ := strconv.ParseInt(parts[0], 10, 64)
		sid, _ := strconv.ParseInt(parts[1], 10, 64)
		name := "user" + parts[0]
		role := perm.Member // token "<user>-<session>-<role>" sets a role
		if len(parts) > 2 {
			role = perm.Role(parts[2])
		}
		hub.Serve(w, r, accounts.Session{ID: sid, User: accounts.User{ID: uid, Username: name, DisplayName: strings.ToUpper(name), Role: role}})
	}))
	t.Cleanup(srv.Close)
	return hub, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// conn is a test client. A background goroutine reads events into a channel, because
// cancelling a Read (e.g. with a timeout) would close the connection.
type conn struct {
	t      *testing.T
	c      *websocket.Conn
	events chan Event
	closed chan websocket.StatusCode // receives the close code when the connection ends
}

// dial connects as user `uid` with session `sid` and returns the connection after its "ready" event.
func dial(t *testing.T, url string, uid, sid int) (*conn, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + strconv.Itoa(uid) + "-" + strconv.Itoa(sid)}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetReadLimit(-1)
	t.Cleanup(func() { c.CloseNow() })
	cn := &conn{t: t, c: c, events: make(chan Event, 10000), closed: make(chan websocket.StatusCode, 1)}
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				cn.closed <- websocket.CloseStatus(err)
				return
			}
			var e Event
			json.Unmarshal(data, &e)
			cn.events <- e
		}
	}()
	ev := cn.next()
	if ev.Type != "ready" {
		t.Fatalf("first event = %q, want ready", ev.Type)
	}
	return cn, ev.Data.(map[string]any)
}

// next returns the next event (or fails after 2 seconds).
func (cn *conn) next() Event {
	cn.t.Helper()
	select {
	case e := <-cn.events:
		return e
	case code := <-cn.closed:
		cn.t.Fatalf("connection closed (code %d) while waiting for an event", code)
	case <-time.After(2 * time.Second):
		cn.t.Fatal("no event within 2 seconds")
	}
	return Event{}
}

// nothing checks that no event arrives within a short time.
func (cn *conn) nothing() {
	cn.t.Helper()
	select {
	case e := <-cn.events:
		cn.t.Errorf("unexpected event: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// closeStatus waits for the connection to end (skipping pending events) and returns its close code.
func (cn *conn) closeStatus() websocket.StatusCode {
	cn.t.Helper()
	for {
		select {
		case <-cn.events:
		case code := <-cn.closed:
			return code
		case <-time.After(10 * time.Second):
			cn.t.Fatal("connection did not close within 10 seconds")
			return 0
		}
	}
}

func (cn *conn) send(s string) {
	cn.t.Helper()
	if err := cn.c.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		cn.t.Fatal(err)
	}
}

func onlineIDs(ready map[string]any) []float64 {
	var ids []float64
	for _, u := range ready["online"].([]any) {
		ids = append(ids, u.(map[string]any)["id"].(float64))
	}
	return ids
}

func TestReadyListsOnlineUsers(t *testing.T) {
	_, url := testServer(t)
	_, ready := dial(t, url, 1, 1)
	if ids := onlineIDs(ready); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("online = %v, want [1] (yourself)", ids)
	}
	if ready["user"].(map[string]any)["display_name"] != "USER1" {
		t.Errorf("ready user = %v", ready["user"])
	}

	_, ready2 := dial(t, url, 2, 2)
	if ids := onlineIDs(ready2); len(ids) != 2 {
		t.Errorf("second user sees online = %v, want 2 users", ids)
	}
}

func TestPresenceOnlineAndOffline(t *testing.T) {
	_, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	b, _ := dial(t, url, 2, 2)

	e := a.next()
	if e.Type != "presence.updated" || e.Data.(map[string]any)["online"] != true {
		t.Fatalf("a got %+v, want user 2 online", e)
	}

	// A second device of the same user does not announce them again.
	b2, _ := dial(t, url, 2, 3)
	a.nothing()

	// Only when the LAST connection of user 2 closes are they offline.
	b.c.Close(websocket.StatusNormalClosure, "")
	a.nothing()
	b2.c.Close(websocket.StatusNormalClosure, "")
	e = a.next()
	data := e.Data.(map[string]any)
	if e.Type != "presence.updated" || data["online"] != false || data["user"].(map[string]any)["id"] != float64(2) {
		t.Errorf("a got %+v, want user 2 offline", e)
	}
}

func TestBroadcastReachesEveryone(t *testing.T) {
	hub, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	b, _ := dial(t, url, 2, 2)
	a.next() // presence of b

	hub.Broadcast("message.created", map[string]any{"id": 7, "content": "hi"})

	for _, c := range []*conn{a, b} {
		e := c.next()
		if e.Type != "message.created" || e.Data.(map[string]any)["content"] != "hi" {
			t.Errorf("got %+v", e)
		}
	}
}

func TestTypingGoesToOthersOnly(t *testing.T) {
	_, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	b, _ := dial(t, url, 2, 2)
	a.next() // presence of b

	a.send(`{"type":"typing","data":{"channel_id":5}}`)
	e := b.next()
	data := e.Data.(map[string]any)
	if e.Type != "typing.started" || data["channel_id"] != float64(5) || data["user"].(map[string]any)["id"] != float64(1) {
		t.Errorf("b got %+v", e)
	}
	a.nothing() // you do not see your own typing

	// Sent again right away: throttled, nobody is notified twice.
	a.send(`{"type":"typing","data":{"channel_id":5}}`)
	b.nothing()
}

func TestBadClientMessagesAreIgnored(t *testing.T) {
	_, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	b, _ := dial(t, url, 2, 2)
	a.next()

	for _, m := range []string{`not json`, `{"type":"unknown"}`, `{"type":"typing","data":{"channel_id":-1}}`, `{"type":"typing"}`} {
		a.send(m)
	}
	b.nothing()
	a.send(`{"type":"typing","data":{"channel_id":1}}`)
	if e := b.next(); e.Type != "typing.started" {
		t.Errorf("connection should still work after ignored messages, got %+v", e)
	}
}

func TestLogoutClosesThatSessionOnly(t *testing.T) {
	hub, url := testServer(t)
	desktop, _ := dial(t, url, 1, 10)
	laptop, _ := dial(t, url, 1, 11)

	hub.EndSession(10)

	if code := desktop.closeStatus(); code != CloseSessionEnded {
		t.Errorf("close code = %d, want %d (session ended)", code, CloseSessionEnded)
	}
	hub.Broadcast("ping.test", nil)
	if e := laptop.next(); e.Type != "ping.test" {
		t.Errorf("other session should stay connected, got %+v", e)
	}
}

func TestTooBigMessageClosesConnection(t *testing.T) {
	_, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	a.send(`{"type":"typing","pad":"` + strings.Repeat("x", maxIncomingBytes+1) + `"}`)
	if code := a.closeStatus(); code != websocket.StatusMessageTooBig {
		t.Errorf("close code = %d, want %d (message too big)", code, websocket.StatusMessageTooBig)
	}
}

func TestConnectionLimitPerUser(t *testing.T) {
	_, url := testServer(t)
	for i := range maxConnectionsPerUser {
		dial(t, url, 1, i+1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer 1-99"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("11th connection: err %v, resp %v; want 429", err, resp)
	}
	dial(t, url, 2, 100) // other users are not affected
}

func TestSlowClientIsDisconnected(t *testing.T) {
	hub, url := testServer(t)
	slow, _ := dial(t, url, 1, 1)

	// The client reads nothing while we send far more than its queue and the network
	// buffers can hold. The hub must drop it instead of blocking.
	big := strings.Repeat("x", 64<<10)
	done := make(chan struct{})
	go func() {
		for range 2000 {
			hub.Broadcast("flood", big)
		}
		close(done)
	}()
	select {
	case <-done: // Broadcast never blocked
	case <-time.After(10 * time.Second):
		t.Fatal("Broadcast blocked on a slow client")
	}
	if code := slow.closeStatus(); code != CloseTooSlow {
		t.Errorf("close code = %d, want %d (too slow)", code, CloseTooSlow)
	}
}

func TestCloseAll(t *testing.T) {
	hub, url := testServer(t)
	a, _ := dial(t, url, 1, 1)

	hub.CloseAll()

	if code := a.closeStatus(); code != websocket.StatusGoingAway {
		t.Errorf("close code = %d, want going away", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer 2-2"}}}); err == nil {
		t.Error("new connection accepted after CloseAll")
	}
}

func TestCrossSiteBrowserPagesAreRejected(t *testing.T) {
	_, url := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A web page on evil.example trying to open a WebSocket to our server.
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer 1-1"}, "Origin": {"https://evil.example"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin connection: err %v, resp %v; want 403", err, resp)
	}
}

// One client must not be able to flood everyone with typing events by using a
// different channel id each time (each id has its own 2-second throttle).
func TestTypingCannotBeAmplifiedWithManyChannelIDs(t *testing.T) {
	_, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	b, _ := dial(t, url, 2, 2)
	a.next() // presence of b

	// 15 ids: within the per-connection message allowance (burst 20), so only the typing limits act.
	for ch := 1; ch <= 15; ch++ {
		a.send(`{"type":"typing","data":{"channel_id":` + strconv.Itoa(ch) + `}}`)
	}
	time.Sleep(300 * time.Millisecond)
	received := 0
	for {
		select {
		case <-b.events:
			received++
			continue
		default:
		}
		break
	}
	if received > 2 {
		t.Errorf("b received %d typing events from 15 sent in a burst; want at most a couple", received)
	}
}

// A client sending messages in a tight loop is disconnected instead of using up CPU.
func TestClientMessageFloodIsDisconnected(t *testing.T) {
	_, url := testServer(t)
	a, _ := dial(t, url, 1, 1)
	for range 500 {
		if a.c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"noop"}`)) != nil {
			break // already closed by the server
		}
	}
	if code := a.closeStatus(); code != websocket.StatusPolicyViolation {
		t.Errorf("close code = %d, want %d (policy violation)", code, websocket.StatusPolicyViolation)
	}
}

func TestEndUserClosesAllTheirConnections(t *testing.T) {
	hub, url := testServer(t)
	desktop, _ := dial(t, url, 7, 1)
	laptop, _ := dial(t, url, 7, 2)
	other, _ := dial(t, url, 8, 3)

	hub.EndUser(7)

	for name, c := range map[string]*conn{"desktop": desktop, "laptop": laptop} {
		if code := c.closeStatus(); code != CloseSessionEnded {
			t.Errorf("%s: close code %d, want %d", name, code, CloseSessionEnded)
		}
	}
	hub.Broadcast("still.here", nil)
	for e := other.next(); e.Type != "still.here"; e = other.next() {
		// skip presence events from the closed connections
	}
}

// dialAs connects with a role ("<uid>-<sid>-<role>" token).
func dialAs(t *testing.T, url string, uid, sid int, role perm.Role) *conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + strconv.Itoa(uid) + "-" + strconv.Itoa(sid) + "-" + string(role)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	cn := &conn{t: t, c: c, events: make(chan Event, 100), closed: make(chan websocket.StatusCode, 1)}
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				cn.closed <- websocket.CloseStatus(err)
				return
			}
			var e Event
			json.Unmarshal(data, &e)
			if e.Type != "ready" && e.Type != "presence.updated" {
				cn.events <- e
			}
		}
	}()
	time.Sleep(100 * time.Millisecond) // let it register
	return cn
}

func TestBroadcastWhereFiltersByRole(t *testing.T) {
	hub, url := testServer(t)
	mod := dialAs(t, url, 1, 1, perm.Moderator)
	member := dialAs(t, url, 2, 2, perm.Member)

	hub.BroadcastWhere("message.created", "staff only", func(r perm.Role) bool { return r.AtLeast(perm.Moderator) })

	if e := mod.next(); e.Type != "message.created" {
		t.Errorf("moderator got %+v", e)
	}
	member.nothing()

	// A role change applies to the open connection at once.
	hub.UpdateUserRole(2, perm.Moderator)
	hub.BroadcastWhere("message.created", "staff only", func(r perm.Role) bool { return r.AtLeast(perm.Moderator) })
	if e := member.next(); e.Type != "message.created" {
		t.Errorf("promoted member got %+v", e)
	}
}

func TestTypingRespectsChannelAccess(t *testing.T) {
	hub, url := testServer(t)
	// Channel 5: moderators can see and write. Channel 6: everyone sees, only admins write.
	hub.SetChannelAccess(func(_ context.Context, id int64) (perm.Role, perm.Role, bool) {
		switch id {
		case 5:
			return perm.Moderator, perm.Moderator, true
		case 6:
			return perm.Member, perm.Admin, true
		}
		return "", "", false
	})
	mod := dialAs(t, url, 1, 1, perm.Moderator)
	member := dialAs(t, url, 2, 2, perm.Member)
	other := dialAs(t, url, 3, 3, perm.Moderator)

	mod.send(`{"type":"typing","data":{"channel_id":5}}`)
	if e := other.next(); e.Type != "typing.started" {
		t.Errorf("moderator should see typing in the staff channel, got %+v", e)
	}
	member.nothing() // never learns anything about the private channel

	member.send(`{"type":"typing","data":{"channel_id":5}}`) // cannot see it
	time.Sleep(1100 * time.Millisecond)
	member.send(`{"type":"typing","data":{"channel_id":6}}`) // can see, cannot write
	time.Sleep(1100 * time.Millisecond)
	member.send(`{"type":"typing","data":{"channel_id":99}}`) // does not exist
	other.nothing()
	mod.nothing()
}

func TestSendToUserReachesOnlyTheirDevices(t *testing.T) {
	hub, url := testServer(t)
	desktop := dialAs(t, url, 7, 1, perm.Member)
	laptop := dialAs(t, url, 7, 2, perm.Member)
	other := dialAs(t, url, 8, 3, perm.Member)

	hub.SendToUser(7, "channel.read", map[string]int64{"channel_id": 1, "last_read_id": 9})
	hub.Broadcast("marker", nil)

	for name, c := range map[string]*conn{"desktop": desktop, "laptop": laptop} {
		if e := c.next(); e.Type != "channel.read" {
			t.Errorf("%s got %+v, want channel.read", name, e)
		}
	}
	if e := other.next(); e.Type != "marker" {
		t.Errorf("another user got %+v before the marker", e)
	}
}

func TestUpdateUserProfileRenamesInTheOnlineList(t *testing.T) {
	hub, url := testServer(t)
	dial(t, url, 7, 1)
	hub.UpdateUserProfile(7, "Renamed")
	_, ready := dial(t, url, 8, 2)
	for _, u := range ready["online"].([]any) {
		info := u.(map[string]any)
		if info["id"] == float64(7) && info["display_name"] != "Renamed" {
			t.Errorf("online list shows %v", info["display_name"])
		}
	}
}

// fakeVoice records what the hub hands to the voice service.
type fakeVoice struct {
	mu    sync.Mutex
	got   []string
	gone  []int64
	peers []Peer
	panic bool
}

func (f *fakeVoice) HandleVoice(p Peer, t string, data json.RawMessage) {
	f.mu.Lock()
	f.got = append(f.got, t+" "+string(data))
	f.peers = append(f.peers, p)
	f.mu.Unlock()
	if f.panic {
		panic("voice bug")
	}
	p.Send("voice.echo", map[string]string{"got": t})
}

func (f *fakeVoice) PeerGone(p Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gone = append(f.gone, p.ConnID())
}

func TestVoiceMessagesGoToTheVoiceService(t *testing.T) {
	hub, url := testServer(t)
	v := &fakeVoice{}
	hub.SetVoice(v)
	a, _ := dial(t, url, 7, 1)

	a.send(`{"type":"voice.join","data":{"channel_id":3}}`)
	if e := a.next(); e.Type != "voice.echo" {
		t.Fatalf("got %+v, want the voice service's reply on the same connection", e)
	}
	v.mu.Lock()
	if len(v.got) != 1 || v.got[0] != `voice.join {"channel_id":3}` || v.peers[0].User().ID != 7 {
		t.Errorf("voice got %v", v.got)
	}
	id := v.peers[0].ConnID()
	v.mu.Unlock()

	a.c.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v.mu.Lock()
		n := len(v.gone)
		v.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.gone) != 1 || v.gone[0] != id {
		t.Errorf("PeerGone calls %v, want [%d]", v.gone, id)
	}
}

func TestAPanicInVoiceKeepsTheConnection(t *testing.T) {
	hub, url := testServer(t)
	hub.SetVoice(&fakeVoice{panic: true})
	a, _ := dial(t, url, 7, 1)
	a.send(`{"type":"voice.join","data":{}}`)
	hub.Broadcast("still.here", nil)
	if e := a.next(); e.Type != "still.here" {
		t.Errorf("after a voice panic got %+v", e)
	}
}
