package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/5cfp/vianden-server/internal/realtime"
)

// Client -> server messages (see docs/API.md, "Voice signaling"):
//
//	voice.join       {"channel_id": 7}          join (or switch to) a voice channel
//	voice.leave      {}                         leave voice
//	voice.answer     {"sdp": "..."}             the client's answer to our last voice.offer
//	voice.candidate  {"candidate": {...}}       one of the client's network addresses
//	voice.self       {"muted": b, "deafened": b} the client's own mute/deafen (shown to others)
//	voice.speaking   {"speaking": b}            the client's microphone level crossed the threshold
//
// Server -> client: voice.joined, voice.offer, voice.candidate, voice.left, voice.error
// (to that connection), voice.state (to everyone who can see the channel), and
// voice.speaking (to the people in the channel).

// HandleVoice is called by the hub for every "voice.*" message (realtime.VoiceHandler).
func (s *Service) HandleVoice(p realtime.Peer, eventType string, data json.RawMessage) {
	defer s.recoverPanic(eventType)
	switch eventType {
	case "voice.join":
		var d struct {
			ChannelID int64 `json:"channel_id"`
		}
		if json.Unmarshal(data, &d) == nil && d.ChannelID > 0 {
			s.join(p, d.ChannelID)
		}
	case "voice.leave":
		s.mu.Lock()
		if part := s.byConn[p.ConnID()]; part != nil {
			s.leaveLocked(part, "left")
		}
		s.mu.Unlock()
	case "voice.answer":
		var d struct {
			SDP string `json:"sdp"`
		}
		if json.Unmarshal(data, &d) == nil && d.SDP != "" && len(d.SDP) <= maxSDPBytes {
			s.answer(p, d.SDP)
		}
	case "voice.candidate":
		var d struct {
			Candidate webrtc.ICECandidateInit `json:"candidate"`
		}
		if json.Unmarshal(data, &d) == nil && d.Candidate.Candidate != "" && len(d.Candidate.Candidate) < 1024 {
			s.mu.Lock()
			if part := s.byConn[p.ConnID()]; part != nil {
				_ = part.pc.AddICECandidate(d.Candidate) // a bad candidate is simply not used
			}
			s.mu.Unlock()
		}
	case "voice.self":
		var d struct {
			Muted    bool `json:"muted"`
			Deafened bool `json:"deafened"`
		}
		if json.Unmarshal(data, &d) == nil {
			s.setSelf(p, d.Muted, d.Deafened)
		}
	case "voice.speaking":
		var d struct {
			Speaking bool `json:"speaking"`
		}
		if json.Unmarshal(data, &d) == nil {
			s.setSpeaking(p, d.Speaking)
		}
	}
}

// PeerGone is called by the hub when a WebSocket connection closes: its voice ends too.
func (s *Service) PeerGone(p realtime.Peer) {
	defer s.recoverPanic("peer gone")
	s.mu.Lock()
	defer s.mu.Unlock()
	if part := s.byConn[p.ConnID()]; part != nil {
		s.leaveLocked(part, "")
	}
}

func sendError(p realtime.Peer, code, message string) {
	p.Send("voice.error", map[string]string{"code": code, "message": message})
}

func (s *Service) join(p realtime.Peer, channelID int64) {
	user := p.User()
	rules, ok := s.channels(context.Background(), channelID)
	// A channel you cannot see does not exist for you (same rule as everywhere else).
	if !ok || !rules.Voice || !user.Role.AtLeast(rules.View) {
		sendError(p, "not_found", "voice channel not found")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.api == nil {
		sendError(p, "voice_unavailable", ErrUnavailable.Error())
		return
	}
	// One voice channel per account: switching channels, or joining from another device,
	// ends the old session.
	if old := s.byConn[p.ConnID()]; old != nil {
		s.leaveLocked(old, "")
	}
	for _, other := range s.byConn {
		if other.user.ID == user.ID {
			s.leaveLocked(other, "joined_elsewhere")
		}
	}
	r := s.rooms[channelID]
	if r == nil {
		r = &room{channelID: channelID}
		s.rooms[channelID] = r
	}
	r.view, r.send = rules.View, rules.Send
	if len(r.members) >= maxPerChannel {
		s.dropIfEmpty(r)
		sendError(p, "channel_full", fmt.Sprintf("a voice channel holds at most %d people", maxPerChannel))
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		s.dropIfEmpty(r)
		s.logger.Error("voice: new peer connection", "error", err)
		sendError(p, "voice_unavailable", "could not start voice")
		return
	}
	part := &participant{peer: p, connID: p.ConnID(), user: info(user), role: user.Role, room: r, pc: pc,
		senders: map[int64]*webrtc.RTPSender{}}
	part.canSpeak.Store(user.Role.AtLeast(rules.Send))

	// The slot for this person's microphone. Listen-only people get it too (simpler for
	// clients); whatever they send is simply not forwarded.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		pc.Close()
		s.dropIfEmpty(r)
		sendError(p, "voice_unavailable", "could not start voice")
		return
	}

	// Pion calls these from its own goroutines. They never take s.mu directly while Pion
	// might hold its own locks; work that needs s.mu runs in a new goroutine.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			p.Send("voice.candidate", map[string]any{"candidate": c.ToJSON()})
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed {
			go s.leaveIfCurrent(part, "connection_lost")
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go s.forward(part, remote)
	})

	r.members = append(r.members, part)
	s.byConn[part.connID] = part
	p.Send("voice.joined", map[string]any{"channel_id": channelID, "can_speak": part.canSpeak.Load()})
	s.negotiateLocked(part)
	s.broadcastStateLocked(r)
}

// negotiateLocked brings one connection up to date (the others' audio added or removed)
// and sends it a new offer. If an offer is already waiting for its answer, it only notes
// that another round is needed (two offers in flight would confuse both sides).
func (s *Service) negotiateLocked(part *participant) {
	if part.awaitingAnswer {
		part.renegotiate = true
		return
	}
	for _, other := range part.room.members {
		if other == part || other.out == nil {
			continue
		}
		if _, ok := part.senders[other.connID]; ok {
			continue
		}
		sender, err := part.pc.AddTrack(other.out)
		if err != nil {
			s.logger.Warn("voice: add track", "error", err)
			continue
		}
		part.senders[other.connID] = sender
		go drainRTCP(sender)
	}
	for connID, sender := range part.senders {
		if other := s.byConn[connID]; other == nil || other.room != part.room || other.out == nil {
			_ = part.pc.RemoveTrack(sender)
			delete(part.senders, connID)
		}
	}

	offer, err := part.pc.CreateOffer(nil)
	if err == nil {
		err = part.pc.SetLocalDescription(offer)
	}
	if err != nil {
		s.logger.Warn("voice: create offer", "error", err)
		return
	}
	part.awaitingAnswer = true
	part.peer.Send("voice.offer", map[string]string{"sdp": offer.SDP})
}

func (s *Service) answer(p realtime.Peer, sdp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	part := s.byConn[p.ConnID()]
	if part == nil || !part.awaitingAnswer {
		return
	}
	if err := part.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		s.logger.Info("voice: bad answer", "user_id", part.user.ID, "error", err)
		s.leaveLocked(part, "connection_lost")
		return
	}
	part.awaitingAnswer = false
	if part.renegotiate {
		part.renegotiate = false
		s.negotiateLocked(part)
	}
}

// forward copies one person's audio packets to their outgoing track (which every other
// connection in the channel has). The server never decodes the audio. Packets are dropped
// while the person may not speak (listen-only, or server-muted): enforced HERE, so a
// modified client cannot get around it.
func (s *Service) forward(part *participant, remote *webrtc.TrackRemote) {
	defer s.recoverPanic("forward")
	if remote.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	out, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, "audio",
		fmt.Sprintf("user-%d", part.user.ID)) // the stream id tells clients whose voice it is
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.byConn[part.connID] != part || part.out != nil {
		s.mu.Unlock()
		return
	}
	part.out = out
	for _, other := range part.room.members {
		if other != part {
			s.negotiateLocked(other)
		}
	}
	s.mu.Unlock()

	for {
		pkt, _, err := remote.ReadRTP()
		if err != nil {
			return // the connection closed
		}
		if part.canSpeak.Load() && !part.serverMuted.Load() {
			if err := out.WriteRTP(pkt); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				return
			}
		}
	}
}

// drainRTCP reads (and discards) the control packets for one outgoing track. Pion's
// interceptors only work if someone reads them.
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

func (s *Service) leaveIfCurrent(part *participant, reason string) {
	defer s.recoverPanic("leave")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byConn[part.connID] == part {
		s.leaveLocked(part, reason)
	}
}

// leaveLocked ends one voice session. reason "" = the connection itself is gone (no
// event to send).
func (s *Service) leaveLocked(part *participant, reason string) {
	if s.byConn[part.connID] != part {
		return
	}
	delete(s.byConn, part.connID)
	r := part.room
	for i, m := range r.members {
		if m == part {
			r.members = append(r.members[:i], r.members[i+1:]...)
			break
		}
	}
	go part.pc.Close() // can take a moment; never hold the lock for it
	if reason != "" {
		part.peer.Send("voice.left", map[string]any{"channel_id": r.channelID, "reason": reason})
	}
	for _, other := range r.members {
		if _, ok := other.senders[part.connID]; ok {
			s.negotiateLocked(other)
		}
	}
	s.broadcastStateLocked(r)
	s.dropIfEmpty(r)
}

func (s *Service) dropIfEmpty(r *room) {
	if len(r.members) == 0 {
		delete(s.rooms, r.channelID)
	}
}

func (s *Service) setSelf(p realtime.Peer, muted, deafened bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	part := s.byConn[p.ConnID()]
	if part == nil || (part.muted == muted && part.deafened == deafened) {
		return
	}
	part.muted, part.deafened = muted, deafened
	if muted || deafened {
		part.speaking = false
	}
	s.broadcastStateLocked(part.room)
}

func (s *Service) setSpeaking(p realtime.Peer, speaking bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	part := s.byConn[p.ConnID()]
	if part == nil || part.speaking == speaking || time.Since(part.lastSpeaking) < speakingEvery {
		return
	}
	// Only people whose audio is actually forwarded can be shown as speaking.
	if speaking && (!part.canSpeak.Load() || part.serverMuted.Load() || part.muted) {
		return
	}
	part.speaking, part.lastSpeaking = speaking, time.Now()
	ev := map[string]any{"channel_id": part.room.channelID, "user_id": part.user.ID, "speaking": speaking}
	for _, m := range part.room.members {
		m.peer.Send("voice.speaking", ev)
	}
}
