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
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/5cfp/vianden-server/internal/accounts"
)

// Limits and timings (also documented in docs/API.md).
const (
	maxConnectionsPerUser = 10               // stops one account from opening thousands of sockets
	maxIncomingBytes      = 4096             // client messages are tiny (only "typing" for now)
	sendQueueSize         = 64               // events waiting for one slow client
	pingInterval          = 30 * time.Second // keeps the connection alive and detects dead ones
	writeTimeout          = 10 * time.Second
	typingThrottle        = 2 * time.Second // one typing event per user and channel at most this often
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
}

type onlineUser struct {
	info        UserInfo
	connections int
}

type client struct {
	user      accounts.User
	sessionID int64
	conn      *websocket.Conn
	send      chan []byte

	// Set (under Hub.mu) once the server has started closing this connection.
	closing bool

	// Only used by this client's reader goroutine.
	lastTyping map[int64]time.Time
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
	}

	h.register(c)
	defer h.unregister(c)
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

// broadcastExceptUser sends to everyone except all connections of one user (e.g. their own typing).
func (h *Hub) broadcastExceptUser(e Event, userID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := mustJSON(e)
	for c := range h.clients {
		if c.user.ID != userID {
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

// incoming is a message from the client. Only "typing" exists for now; others are ignored,
// so newer clients can talk to this server without breaking it.
type incoming struct {
	Type string `json:"type"`
	Data struct {
		ChannelID int64 `json:"channel_id"`
	} `json:"data"`
}

func (h *Hub) readLoop(c *client) {
	defer h.recoverPanic(c)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return // closed by either side, or a message over the read limit
		}
		var in incoming
		if json.Unmarshal(data, &in) != nil {
			continue // not JSON: ignore
		}
		if in.Type == "typing" && in.Data.ChannelID > 0 {
			h.handleTyping(c, in.Data.ChannelID)
		}
	}
}

func (h *Hub) handleTyping(c *client, channelID int64) {
	now := time.Now()
	if now.Sub(c.lastTyping[channelID]) < typingThrottle {
		return // a client sending typing events in a loop cannot flood everyone else
	}
	c.lastTyping[channelID] = now
	h.broadcastExceptUser(Event{"typing.started", map[string]any{"channel_id": channelID, "user": userInfo(c.user)}}, c.user.ID)
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
