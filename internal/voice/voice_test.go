package voice

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
	"github.com/5cfp/vianden-server/internal/tcpshare"
)

// ---- fakes for the hub side ----

type event struct {
	Type string
	Data json.RawMessage
}

// fakePeer is one "WebSocket connection": events from the server go into a channel.
type fakePeer struct {
	id     int64
	mu     sync.Mutex
	user   accounts.User
	events chan event
}

func (p *fakePeer) ConnID() int64 { return p.id }
func (p *fakePeer) User() accounts.User {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.user
}
func (p *fakePeer) Send(t string, data any) bool {
	b, _ := json.Marshal(data)
	select {
	case p.events <- event{t, b}:
	default: // a test that does not read events must not block the server
	}
	return true
}

type broadcast struct {
	Type string
	Data any
}

type fakeBroadcaster struct {
	mu   sync.Mutex
	sent []broadcast
}

func (b *fakeBroadcaster) BroadcastWhere(t string, data any, _ func(perm.Role) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, broadcast{t, data})
}

func (b *fakeBroadcaster) last() ChannelState {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.sent) - 1; i >= 0; i-- {
		if st, ok := b.sent[i].Data.(ChannelState); ok {
			return st
		}
	}
	return ChannelState{}
}

// Channels: 1 = open voice room, 2 = members listen, moderators speak, 3 = text,
// 4 = admins only.
func lookup(_ context.Context, id int64) (ChannelRules, bool) {
	switch id {
	case 1:
		return ChannelRules{View: perm.Member, Send: perm.Member, Voice: true}, true
	case 2:
		return ChannelRules{View: perm.Member, Send: perm.Moderator, Voice: true}, true
	case 3:
		return ChannelRules{View: perm.Member, Send: perm.Member}, true
	case 4:
		return ChannelRules{View: perm.Admin, Send: perm.Admin, Voice: true}, true
	}
	return ChannelRules{}, false
}

func newService(t *testing.T, tcp net.Listener) (*Service, *fakeBroadcaster) {
	t.Helper()
	bc := &fakeBroadcaster{}
	s, err := New(context.Background(), Config{UDPPort: 0, TCP: tcp, Loopback: true}, lookup, bc,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, bc
}

// ---- a test client: a real WebRTC endpoint (Pion), like the app ----

type client struct {
	t    *testing.T
	s    *Service
	peer *fakePeer
	pc   *webrtc.PeerConnection
	mic  *webrtc.TrackLocalStaticRTP

	mu       sync.Mutex
	streams  []string // stream ids of incoming audio ("user-<id>")
	received map[byte]int
	other    []event // events that are not signaling
}

var nextConn int64 = 100

func newClient(t *testing.T, s *Service, user accounts.User, networks ...webrtc.NetworkType) *client {
	t.Helper()
	nextConn++
	c := &client{t: t, s: s, peer: &fakePeer{id: nextConn, user: user, events: make(chan event, 500)}, received: map[byte]int{}}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	ir := &interceptor.Registry{}
	webrtc.RegisterDefaultInterceptors(m, ir)
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	if len(networks) == 0 {
		networks = []webrtc.NetworkType{webrtc.NetworkTypeUDP4}
	}
	se.SetNetworkTypes(networks)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	c.pc = pc
	c.mic, _ = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "me")
	if _, err := pc.AddTrack(c.mic); err != nil {
		t.Fatal(err)
	}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			b, _ := json.Marshal(map[string]any{"candidate": cand.ToJSON()})
			s.HandleVoice(c.peer, "voice.candidate", b)
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.mu.Lock()
		c.streams = append(c.streams, track.StreamID())
		c.mu.Unlock()
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(pkt.Payload) > 0 {
				c.mu.Lock()
				c.received[pkt.Payload[0]]++
				c.mu.Unlock()
			}
		}
	})
	go c.signal()
	return c
}

// signal answers the server's offers and adds its candidates, like the app does.
func (c *client) signal() {
	for e := range c.peer.events {
		switch e.Type {
		case "voice.offer":
			var d struct{ SDP string }
			json.Unmarshal(e.Data, &d)
			if c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP}) != nil {
				continue
			}
			answer, err := c.pc.CreateAnswer(nil)
			if err != nil || c.pc.SetLocalDescription(answer) != nil {
				continue
			}
			b, _ := json.Marshal(map[string]string{"sdp": answer.SDP})
			c.s.HandleVoice(c.peer, "voice.answer", b)
		case "voice.candidate":
			var d struct{ Candidate webrtc.ICECandidateInit }
			json.Unmarshal(e.Data, &d)
			c.pc.AddICECandidate(d.Candidate)
		default:
			c.mu.Lock()
			c.other = append(c.other, e)
			c.mu.Unlock()
		}
	}
}

func (c *client) join(channelID int64) {
	b, _ := json.Marshal(map[string]int64{"channel_id": channelID})
	c.s.HandleVoice(c.peer, "voice.join", b)
}

// talk sends fake audio packets whose first payload byte is `mark`, until stop is closed.
func (c *client) talk(mark byte, stop chan struct{}) {
	go func() {
		seq := uint16(0)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				seq++
				c.mic.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq,
					Timestamp: uint32(seq) * 960}, Payload: []byte{mark, 0xF8, 0xFF, 0xFE}})
			}
		}
	}()
}

func (c *client) heard(mark byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.received[mark]
}

// eventually waits until cond is true (network tests need a moment).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func (c *client) got(eventType string) *event {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.other) - 1; i >= 0; i-- {
		if c.other[i].Type == eventType {
			return &c.other[i]
		}
	}
	return nil
}

var (
	alice = accounts.User{ID: 1, Username: "alice", DisplayName: "Alice", Role: perm.Member}
	bob   = accounts.User{ID: 2, Username: "bob", DisplayName: "Bob", Role: perm.Member}
	mod   = accounts.User{ID: 3, Username: "mod", DisplayName: "Mod", Role: perm.Moderator}
)

// ---- tests ----

func TestTwoPeopleHearEachOther(t *testing.T) {
	s, bc := newService(t, nil)
	a := newClient(t, s, alice)
	b := newClient(t, s, bob)
	a.join(1)
	b.join(1)

	stop := make(chan struct{})
	defer close(stop)
	a.talk('A', stop)
	b.talk('B', stop)

	eventually(t, "bob hears alice", func() bool { return b.heard('A') > 5 })
	eventually(t, "alice hears bob", func() bool { return a.heard('B') > 5 })
	if a.heard('A') != 0 {
		t.Error("alice hears herself")
	}
	b.mu.Lock()
	if len(b.streams) == 0 || b.streams[0] != "user-1" {
		t.Errorf("bob's incoming streams %v: want user-1 (tells the app whose voice it is)", b.streams)
	}
	b.mu.Unlock()
	if st := bc.last(); len(st.Participants) != 2 || st.Participants[0].User.ID != 1 {
		t.Errorf("state %+v", st)
	}
	if j := a.got("voice.joined"); j == nil || string(j.Data) != `{"can_speak":true,"channel_id":1}` {
		t.Errorf("joined event %+v", j)
	}
}

func TestListenOnlyAudioIsNotForwarded(t *testing.T) {
	s, bc := newService(t, nil)
	listener := newClient(t, s, alice) // member: may only listen in channel 2
	speaker := newClient(t, s, mod)
	listener.join(2)
	speaker.join(2)

	stop := make(chan struct{})
	defer close(stop)
	listener.talk('L', stop) // a modified app sending audio anyway
	speaker.talk('S', stop)

	eventually(t, "the member hears the moderator", func() bool { return listener.heard('S') > 5 })
	time.Sleep(500 * time.Millisecond)
	if n := speaker.heard('L'); n != 0 {
		t.Errorf("listen-only audio was forwarded (%d packets)", n)
	}
	if st := bc.last(); st.Participants[0].CanSpeak {
		t.Errorf("state says the member can speak: %+v", st)
	}
}

func TestServerMuteStopsForwarding(t *testing.T) {
	s, bc := newService(t, nil)
	a := newClient(t, s, alice)
	m := newClient(t, s, mod)
	a.join(1)
	m.join(1)
	stop := make(chan struct{})
	defer close(stop)
	a.talk('A', stop)
	eventually(t, "mod hears alice", func() bool { return m.heard('A') > 5 })

	if err := s.SetServerMute(alice, 1, mod.ID, true); err == nil {
		t.Error("a member server-muted a moderator")
	}
	if err := s.SetServerMute(mod, 1, alice.ID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // packets already on the way
	before := m.heard('A')
	time.Sleep(500 * time.Millisecond)
	if after := m.heard('A'); after > before+2 {
		t.Errorf("still forwarding after server mute: %d -> %d", before, after)
	}
	if st := bc.last(); !st.Participants[0].ServerMuted {
		t.Errorf("state: %+v", st)
	}

	s.SetServerMute(mod, 1, alice.ID, false)
	eventually(t, "forwarding again after unmute", func() bool { return m.heard('A') > before+10 })
}

func TestDisconnect(t *testing.T) {
	s, bc := newService(t, nil)
	a := newClient(t, s, alice)
	b := newClient(t, s, bob)
	a.join(1)
	b.join(1)
	eventually(t, "both in", func() bool { return len(bc.last().Participants) == 2 })

	if err := s.Disconnect(bob, 1, alice.ID); err == nil {
		t.Error("a member disconnected another member")
	}
	if err := s.Disconnect(mod, 1, 999); err != ErrNotInVoice {
		t.Errorf("unknown user: %v", err)
	}
	if err := s.Disconnect(mod, 1, alice.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice told why", func() bool {
		e := a.got("voice.left")
		return e != nil && string(e.Data) == `{"channel_id":1,"reason":"disconnected"}`
	})
	if st := bc.last(); len(st.Participants) != 1 || st.Participants[0].User.ID != bob.ID {
		t.Errorf("state after disconnect: %+v", st)
	}
}

func TestCannotJoinWhatYouCannotSee(t *testing.T) {
	s, _ := newService(t, nil)
	a := newClient(t, s, alice)
	for _, id := range []int64{3, 4, 99} { // text, admins only, missing
		a.join(id)
	}
	eventually(t, "errors", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.other) == 3
	})
	for _, e := range a.other {
		if e.Type != "voice.error" || string(e.Data) != `{"code":"not_found","message":"voice channel not found"}` {
			t.Errorf("got %s %s", e.Type, e.Data)
		}
	}
	if len(s.List(context.Background(), alice)) != 0 {
		t.Error("someone joined")
	}
}

func TestJoiningElsewhereEndsTheOldSession(t *testing.T) {
	s, _ := newService(t, nil)
	desktop := newClient(t, s, alice)
	laptop := newClient(t, s, alice)
	desktop.join(1)
	laptop.join(1)
	eventually(t, "desktop told", func() bool {
		e := desktop.got("voice.left")
		return e != nil && string(e.Data) == `{"channel_id":1,"reason":"joined_elsewhere"}`
	})
	if l := s.List(context.Background(), alice); len(l) != 1 || len(l[0].Participants) != 1 {
		t.Errorf("list %+v", l)
	}
}

func TestRoleChangesApplyLive(t *testing.T) {
	s, bc := newService(t, nil)
	a := newClient(t, s, alice)
	a.join(2) // member: listen only
	eventually(t, "joined", func() bool { return len(bc.last().Participants) == 1 })

	a.peer.mu.Lock()
	a.peer.user.Role = perm.Moderator
	a.peer.mu.Unlock()
	s.UserChanged(alice.ID)
	if st := bc.last(); !st.Participants[0].CanSpeak {
		t.Errorf("promoted but still listen-only: %+v", st)
	}

	s.ChannelDeleted(2)
	eventually(t, "removed with the channel", func() bool {
		e := a.got("voice.left")
		return e != nil && string(e.Data) == `{"channel_id":2,"reason":"channel_deleted"}`
	})
}

func TestPeerGoneLeaves(t *testing.T) {
	s, bc := newService(t, nil)
	a := newClient(t, s, alice)
	a.join(1)
	eventually(t, "joined", func() bool { return len(bc.last().Participants) == 1 })
	s.PeerGone(a.peer)
	if len(bc.last().Participants) != 0 || len(s.List(context.Background(), alice)) != 0 {
		t.Error("still in voice after the connection closed")
	}
}

// Voice over TCP (networks that block UDP): the client may only use TCP, and connects
// through the shared main port (tcpshare), like on port 443.
func TestVoiceOverTCPFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	q := tcpshare.NewQueue(ln.Addr().(*net.TCPAddr))
	web := tcpshare.Split(ln, q)
	defer web.Close()

	s, _ := newService(t, q)
	a := newClient(t, s, alice, webrtc.NetworkTypeTCP4)
	b := newClient(t, s, bob, webrtc.NetworkTypeTCP4)
	a.join(1)
	b.join(1)
	stop := make(chan struct{})
	defer close(stop)
	a.talk('A', stop)
	eventually(t, "audio over TCP", func() bool { return b.heard('A') > 5 })
}

func TestResolve(t *testing.T) {
	ips, err := resolve(context.Background(), []string{"192.168.1.20", "203.0.113.5", "192.168.1.20"})
	if err != nil || len(ips) != 2 || ips[0] != "192.168.1.20" {
		t.Errorf("resolve: %v %v", ips, err)
	}
	if _, err := resolve(context.Background(), []string{"2001:db8::1"}); err == nil {
		t.Error("IPv6 accepted")
	}
	if ips, err := resolve(context.Background(), []string{"localhost"}); err != nil || len(ips) == 0 {
		t.Errorf("host name: %v %v", ips, err)
	}
}
