// Package voice is the audio-only SFU (M7), built with Pion. It started from Pion's
// official "sfu-ws" example, reduced to audio and adapted to Vianden's channels,
// permissions and WebSocket.
//
// Signaling (setting up the WebRTC connection) runs over the existing WebSocket: the hub
// passes every "voice.*" message here (see realtime.VoiceHandler). The server always makes
// the offers; clients only answer. That keeps renegotiation simple: when someone joins or
// leaves, the server sends everyone in the channel a new offer.
//
// Fault isolation: everything here recovers from panics, and if the UDP port cannot be
// opened, voice is simply unavailable; text chat keeps working.
package voice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
	"github.com/5cfp/vianden-server/internal/realtime"
)

const (
	maxPerChannel = 25                     // people in one voice channel
	speakingEvery = 150 * time.Millisecond // "speaking" updates per person at most this often
	maxSDPBytes   = 24 << 10
)

var (
	ErrNotInVoice  = errors.New("that user is not in this voice channel")
	ErrUnavailable = errors.New("voice is not available on this server")
)

// Config is how voice reaches the network.
type Config struct {
	UDPPort         int
	PublicAddresses []string     // IPs or host names to tell clients (empty: the machine's own)
	TCP             net.Listener // ICE-TCP fallback connections (nil = none); see tcpshare
	Loopback        bool         // also offer 127.0.0.1 (tests only)
}

// ChannelRules is what voice needs to know about a channel.
type ChannelRules struct {
	View, Send perm.Role
	Voice      bool // false for text channels
}

// ChannelLookup returns a channel's rules (ok=false if it does not exist).
type ChannelLookup func(ctx context.Context, id int64) (ChannelRules, bool)

// Broadcaster sends an event to every connection whose user role passes `to`.
type Broadcaster interface {
	BroadcastWhere(eventType string, data any, to func(perm.Role) bool)
}

type Service struct {
	cfg      Config
	logger   *slog.Logger
	channels ChannelLookup
	bc       Broadcaster

	udpConn net.PacketConn
	udpMux  ice.UDPMux
	tcpMux  ice.TCPMux

	mu        sync.Mutex
	api       *webrtc.API // nil = voice unavailable
	publicIPs []string
	rooms     map[int64]*room        // by channel id
	byConn    map[int64]*participant // by WebSocket connection id
}

type room struct {
	channelID  int64
	view, send perm.Role
	members    []*participant // in joining order
}

type participant struct {
	peer   realtime.Peer
	connID int64
	user   userInfo
	role   perm.Role
	room   *room
	pc     *webrtc.PeerConnection

	// out carries this person's audio to the others; nil until it arrives.
	out *webrtc.TrackLocalStaticRTP
	// senders: the other people's audio on THIS person's connection, by their conn id.
	senders map[int64]*webrtc.RTPSender

	// Read by the forwarding goroutine without the lock, so atomic.
	canSpeak    atomic.Bool
	serverMuted atomic.Bool

	muted, deafened, speaking bool
	lastSpeaking              time.Time

	// Offer/answer bookkeeping: never two offers in flight on one connection.
	awaitingAnswer, renegotiate bool
}

type userInfo struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// New opens the UDP port and prepares voice. If the port cannot be opened, it returns
// an error; the caller logs it and runs without voice.
func New(ctx context.Context, cfg Config, channels ChannelLookup, bc Broadcaster, logger *slog.Logger) (*Service, error) {
	s := &Service{cfg: cfg, logger: logger, channels: channels, bc: bc,
		rooms: map[int64]*room{}, byConn: map[int64]*participant{}}
	if cfg.UDPPort == 0 && !cfg.Loopback {
		return nil, ErrUnavailable // turned off in the config
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.UDPPort})
	if err != nil {
		return nil, fmt.Errorf("opening the voice UDP port %d: %w", cfg.UDPPort, err)
	}
	s.udpConn = udp
	s.udpMux = webrtc.NewICEUDPMux(nil, udp)
	if cfg.TCP != nil {
		s.tcpMux = webrtc.NewICETCPMux(nil, cfg.TCP, 8)
	}
	if err := s.refreshAddresses(ctx); err != nil {
		udp.Close()
		return nil, err
	}
	return s, nil
}

// UDPAddr is where voice listens (tests use it to find a port chosen by the system).
func (s *Service) UDPAddr() net.Addr { return s.udpConn.LocalAddr() }

// Run keeps the public addresses up to date until ctx ends (run it under the supervisor).
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(addressRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.refreshAddresses(ctx); err != nil {
				s.logger.Warn("voice: could not refresh public addresses", "error", err)
			}
		}
	}
}

// refreshAddresses looks up the public addresses and, if they changed, builds a new Pion
// API with them (used for connections from now on).
func (s *Service) refreshAddresses(ctx context.Context) error {
	ips, err := resolve(ctx, s.cfg.PublicAddresses)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.api != nil && slices.Equal(ips, s.publicIPs) {
		return nil
	}
	api, err := s.newAPI(ips)
	if err != nil {
		return err
	}
	s.api, s.publicIPs = api, ips
	s.logger.Info("voice ready", "udp_port", s.udpConn.LocalAddr(), "public_ips", ips, "tcp_fallback", s.tcpMux != nil)
	return nil
}

// Available reports whether voice works on this server (for GET /info).
func (s *Service) Available() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.api != nil
}

// Close ends every voice session and closes the sockets.
func (s *Service) Close() {
	s.mu.Lock()
	for _, p := range s.byConn {
		s.leaveLocked(p, "server_stopping")
	}
	s.api = nil
	s.mu.Unlock()
	if s.tcpMux != nil {
		s.tcpMux.Close()
	}
	s.udpMux.Close()
	s.udpConn.Close()
}

// recoverPanic logs a panic in voice instead of crashing the server.
func (s *Service) recoverPanic(where string) {
	if v := recover(); v != nil {
		s.logger.Error("panic in voice", "where", where, "panic", v, "stack", string(debug.Stack()))
	}
}

func info(u accounts.User) userInfo {
	return userInfo{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName}
}
