// Package realtime pushes live events (new messages, presence, typing) to connected
// clients over WebSockets. Every event is documented in docs/API.md, section 5.
//
// How it works: each WebSocket connection is a "client" with two goroutines, one
// reading what the client sends and one writing events to it. The Hub keeps the list
// of clients. To send an event to everyone, the Hub puts it in each client's
// outgoing queue; the client's writer sends it. A client that cannot keep up (its
// queue is full) is disconnected, so one slow connection never slows down the rest.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
)

// Limits and timings (also documented in docs/API.md).
const (
	maxConnectionsPerUser = 10               // stops one account from opening thousands of sockets
	maxIncomingBytes      = 32 << 10         // client messages are small; voice answers (SDP) are the biggest
	sendQueueSize         = 64               // events waiting for one slow client
	pingInterval          = 30 * time.Second // keeps the connection alive and detects dead ones
	writeTimeout          = 10 * time.Second
	typingThrottle        = 2 * time.Second // one typing event per user and channel at most this often
	typingThrottleAny     = time.Second     // and at most one per connection per second, across all channels
	// Client messages per connection: 20 per second on average, bursts of 60 (joining voice
	// sends a quick burst of network candidates). More than that is not a normal app, so
	// the connection is closed (policy violation, 1008).
	incomingPerSecond = 20
	incomingBurst     = 60
)

// Custom close codes (4000-4999 are free for applications).
const (
	// CloseSessionEnded: the session was logged out or revoked. The client must log in again.
	CloseSessionEnded websocket.StatusCode = 4001
	// CloseTooSlow: the client did not read events fast enough.
	CloseTooSlow websocket.StatusCode = 4008
)

// Event is what goes over the wire: {"type": "...", "data": {...}}.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// UserInfo is the public view of a user inside events.
type UserInfo struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

func userInfo(u accounts.User) UserInfo {
	return UserInfo{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName}
}

type Hub struct {
	logger *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
	online  map[int64]*onlineUser // user id -> info and number of open connections
	closed  bool                  // true after CloseAll: no new connections

	channelAccess ChannelAccessFunc // nil = no channel checks (tests)
	voice         VoiceHandler      // nil = voice messages are ignored
	nextID        int64
}

type onlineUser struct {
	info        UserInfo
	connections int
}

type client struct {
	id        int64 // unique per connection (voice sessions belong to one connection)
	user      accounts.User
	sessionID int64
	conn      *websocket.Conn
	send      chan []byte

	// Set (under Hub.mu) once the server has started closing this connection.
	closing bool

	// Only used by this client's reader goroutine.
	lastTyping    map[int64]time.Time
	lastTypingAny time.Time
	incoming      *rate.Limiter
}

func NewHub(logger *slog.Logger) *Hub {
	return &Hub{logger: logger, clients: make(map[*client]struct{}), online: make(map[int64]*onlineUser)}
}

// Serve upgrades an authenticated HTTP request to a WebSocket and runs it until it closes.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, s accounts.Session) {
	if err := h.checkCanConnect(s.User.ID); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"too_many_connections","message":"too many open connections for this account"}}` + "\n"))
		return
	}

	// Accept rejects requests from web pages on other sites (it checks the Origin header).
	// That blocks "cross-site WebSocket hijacking". Native apps send no Origin and are fine.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already answered the request
	}
	conn.SetReadLimit(maxIncomingBytes)

	c := &client{
		user:       s.User,
		sessionID:  s.ID,
		conn:       conn,
		send:       make(chan []byte, sendQueueSize),
		lastTyping: make(map[int64]time.Time),
		incoming:   rate.NewLimiter(incomingPerSecond, incomingBurst),
	}

	h.register(c)
	defer func() {
		h.unregister(c)
		// Outside the lock: voice may send events to other connections while cleaning up.
		if v := h.voiceHandler(); v != nil {
			h.safeVoice(c, func() { v.PeerGone(peer{h, c}) })
		}
	}()
	h.logger.Info("websocket connected", "user_id", s.User.ID)

	// Note: cancelling a context given to conn.Read would drop the connection WITHOUT a close
	// code, so the reader gets a context that is never cancelled. Connections are ended with
	// conn.Close (which sends a close code the client can act on), and that also ends Read.
	writerCtx, stopWriter := context.WithCancel(context.Background())
	go h.writeLoop(writerCtx, c)
	h.readLoop(c) // returns when the connection is closed, by either side
	stopWriter()
	conn.CloseNow() // make sure it is fully closed (does nothing if already closed)
	h.logger.Info("websocket disconnected", "user_id", s.User.ID)
}

func (h *Hub) checkCanConnect(userID int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("shutting down")
	}
	if u := h.online[userID]; u != nil && u.connections >= maxConnectionsPerUser {
		return errors.New("too many connections")
	}
	return nil
}

// register adds a client, sends it the "ready" event, and tells everyone else if the user just came online.
func (h *Hub) register(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	c.id = h.nextID
	h.clients[c] = struct{}{}
	u := h.online[c.user.ID]
	cameOnline := u == nil
	if cameOnline {
		u = &onlineUser{info: userInfo(c.user)}
		h.online[c.user.ID] = u
	}
	u.connections++

	others := make([]UserInfo, 0, len(h.online))
	for _, o := range h.online {
		others = append(others, o.info)
	}
	h.enqueue(c, mustJSON(Event{"ready", map[string]any{"user": userInfo(c.user), "online": others}}))

	if cameOnline {
		h.broadcastLocked(Event{"presence.updated", map[string]any{"user": u.info, "online": true}}, c)
	}
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if u := h.online[c.user.ID]; u != nil {
		u.connections--
		if u.connections == 0 {
			delete(h.online, c.user.ID)
			h.broadcastLocked(Event{"presence.updated", map[string]any{"user": u.info, "online": false}}, nil)
		}
	}
}

// Broadcast sends an event to every connected client.
func (h *Hub) Broadcast(eventType string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastLocked(Event{eventType, data}, nil)
}

// broadcastLocked sends to everyone except `skip` (may be nil). Caller holds h.mu.
func (h *Hub) broadcastLocked(e Event, skip *client) {
	msg := mustJSON(e) // encoded once, sent to everyone
	for c := range h.clients {
		if c != skip {
			h.enqueue(c, msg)
		}
	}
}

// enqueue puts a message in a client's queue without ever waiting. Caller holds h.mu.
func (h *Hub) enqueue(c *client, msg []byte) {
	select {
	case c.send <- msg:
	default:
		h.closeLocked(c, CloseTooSlow, "too slow")
	}
}

// closeLocked closes a client's connection with the given code (once). Caller holds h.mu.
// Close waits for the client to confirm, so it runs in its own goroutine to never block the hub.
func (h *Hub) closeLocked(c *client, code websocket.StatusCode, reason string) {
	if c.closing {
		return
	}
	c.closing = true
	go c.conn.Close(code, reason)
}

// EndSession closes every connection that belongs to a session (called on logout).
func (h *Hub) EndSession(sessionID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.sessionID == sessionID {
			h.closeLocked(c, CloseSessionEnded, "session ended")
		}
	}
}

// CloseAll disconnects everyone and refuses new connections (server shutdown).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		h.closeLocked(c, websocket.StatusGoingAway, "server shutting down")
	}
}

// writeLoop sends queued events and regular pings until ctx is cancelled or a write fails.
func (h *Hub) writeLoop(ctx context.Context, c *client) {
	defer h.recoverPanic(c)
	defer c.conn.CloseNow() // a failed write or ping means the connection is dead: end it
	conn := c.conn

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			wctx, done := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, msg)
			done()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, done := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx) // no answer in time: the connection is dead
			done()
			if err != nil {
				return
			}
		}
	}
}

// incoming is a message from the client: "typing", or "voice.*" (M7). Others are ignored,
// so newer clients can talk to this server without breaking it.
type incoming struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (h *Hub) readLoop(c *client) {
	defer h.recoverPanic(c)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return // closed by either side, or a message over the read limit
		}
		if !c.incoming.Allow() {
			// Closed right here (not in a goroutine): the close message must be sent before
			// Serve cleans up, so the client learns why (code 1008).
			h.mu.Lock()
			first := !c.closing
			c.closing = true
			h.mu.Unlock()
			if first {
				c.conn.Close(websocket.StatusPolicyViolation, "too many messages")
			}
			return
		}
		var in incoming
		if json.Unmarshal(data, &in) != nil {
			continue // not JSON: ignore
		}
		switch {
		case in.Type == "typing":
			var d struct {
				ChannelID int64 `json:"channel_id"`
			}
			if json.Unmarshal(in.Data, &d) == nil && d.ChannelID > 0 {
				h.handleTyping(c, d.ChannelID)
			}
		case strings.HasPrefix(in.Type, "voice."):
			if v := h.voiceHandler(); v != nil {
				h.safeVoice(c, func() { v.HandleVoice(peer{h, c}, in.Type, in.Data) })
			}
		}
	}
}

func (h *Hub) handleTyping(c *client, channelID int64) {
	now := time.Now()
	// Two limits: per channel, and overall. Without the overall one, a client could send
	// typing for channel 1, 2, 3, ... and every id would pass its own per-channel limit,
	// flooding everyone else with events (one sender, many receivers = amplification).
	if now.Sub(c.lastTyping[channelID]) < typingThrottle || now.Sub(c.lastTypingAny) < typingThrottleAny {
		return
	}
	c.lastTyping[channelID] = now
	c.lastTypingAny = now

	h.mu.Lock()
	access, sender := h.channelAccess, c.user // read under the lock: the role can change
	h.mu.Unlock()

	visibleTo := func(perm.Role) bool { return true }
	if access != nil {
		view, send, ok := access(context.Background(), channelID)
		// Typing is only meaningful where the sender may write, and only shown to people
		// who may see the channel. Unknown channels are ignored.
		if !ok || !sender.Role.AtLeast(view) || !sender.Role.AtLeast(send) {
			return
		}
		visibleTo = func(r perm.Role) bool { return r.AtLeast(view) }
	}

	ev := mustJSON(Event{"typing.started", map[string]any{"channel_id": channelID, "user": userInfo(sender)}})
	h.mu.Lock()
	defer h.mu.Unlock()
	for other := range h.clients {
		if other.user.ID != sender.ID && visibleTo(other.user.Role) {
			h.enqueue(other, ev)
		}
	}
}

// recoverPanic stops a panic in one connection from crashing the server; the connection is closed.
func (h *Hub) recoverPanic(c *client) {
	if v := recover(); v != nil {
		h.logger.Error("panic in websocket connection", "user_id", c.user.ID, "panic", v, "stack", string(debug.Stack()))
		c.conn.CloseNow()
	}
}

func mustJSON(e Event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // only our own event structs are encoded: this is a programming error
	}
	return b
}

// EndUser closes every connection of a user (kick, ban). The client receives 4001 and
// shows the login screen.
func (h *Hub) EndUser(userID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.user.ID == userID {
			h.closeLocked(c, CloseSessionEnded, "signed out by a moderator")
		}
	}
}

// UpdateUserRole updates the role of a user's open connections, so what they may see
// (private channels, M5) changes at once, without reconnecting.
func (h *Hub) UpdateUserRole(userID int64, role perm.Role) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.user.ID == userID {
			c.user.Role = role
		}
	}
}

// BroadcastWhere sends an event only to connections whose user's role passes `to`.
// Used for private channels: a message in a moderators-only room never reaches members.
func (h *Hub) BroadcastWhere(eventType string, data any, to func(perm.Role) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := mustJSON(Event{eventType, data})
	for c := range h.clients {
		if to(c.user.Role) {
			h.enqueue(c, msg)
		}
	}
}

// ChannelAccessFunc returns a channel's minimum roles to see and to write (ok=false if it
// does not exist). The server passes chat.Service.ChannelAccess.
type ChannelAccessFunc func(ctx context.Context, channelID int64) (view, send perm.Role, ok bool)

// SetChannelAccess enables access checks for typing events. Without it (tests), typing is
// sent to everyone.
func (h *Hub) SetChannelAccess(f ChannelAccessFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.channelAccess = f
}

// SendToUser sends an event to every open connection of one user (their other devices),
// e.g. "you read this channel" so all of them clear the unread badge.
func (h *Hub) SendToUser(userID int64, eventType string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := mustJSON(Event{eventType, data})
	for c := range h.clients {
		if c.user.ID == userID {
			h.enqueue(c, msg)
		}
	}
}

// UpdateUserProfile changes a user's display name on their open connections and in the
// online list, so presence events and typing show the new name.
func (h *Hub) UpdateUserProfile(userID int64, displayName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.user.ID == userID {
			c.user.DisplayName = displayName
		}
	}
	if o := h.online[userID]; o != nil {
		o.info.DisplayName = displayName
	}
}

// ---- voice (M7) ----

// Peer is one WebSocket connection, as the voice service sees it. A voice session belongs
// to exactly one connection: when it closes, the session ends.
type Peer interface {
	ConnID() int64
	User() accounts.User // current: the role can change while connected
	// Send queues an event for this connection only; false if it is already gone.
	Send(eventType string, data any) bool
}

// VoiceHandler receives the "voice.*" messages of every connection, and is told when a
// connection closes. The voice service implements it.
type VoiceHandler interface {
	HandleVoice(p Peer, eventType string, data json.RawMessage)
	PeerGone(p Peer)
}

// SetVoice connects the voice service (nil: voice messages are ignored).
func (h *Hub) SetVoice(v VoiceHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.voice = v
}

func (h *Hub) voiceHandler() VoiceHandler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.voice
}

// safeVoice runs a call into the voice service. A panic in voice is logged and the
// connection stays open: voice failing must not break text chat.
func (h *Hub) safeVoice(c *client, call func()) {
	defer func() {
		if v := recover(); v != nil {
			h.logger.Error("panic in voice", "user_id", c.user.ID, "panic", v, "stack", string(debug.Stack()))
		}
	}()
	call()
}

type peer struct {
	h *Hub
	c *client
}

func (p peer) ConnID() int64 { return p.c.id }

func (p peer) User() accounts.User {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	return p.c.user
}

func (p peer) Send(eventType string, data any) bool {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	if _, ok := p.h.clients[p.c]; !ok || p.c.closing {
		return false
	}
	p.h.enqueue(p.c, mustJSON(Event{eventType, data}))
	return true
}
