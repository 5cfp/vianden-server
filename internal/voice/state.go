package voice

import (
	"context"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
)

// ParticipantState is one person in a voice channel, as clients see it.
type ParticipantState struct {
	User        userInfo `json:"user"`
	Muted       bool     `json:"muted"`        // muted themselves
	Deafened    bool     `json:"deafened"`     // turned off their sound
	ServerMuted bool     `json:"server_muted"` // muted by a moderator: their audio is not forwarded
	CanSpeak    bool     `json:"can_speak"`    // their role may speak in this channel
}

// ChannelState is who is in one voice channel.
type ChannelState struct {
	ChannelID    int64              `json:"channel_id"`
	Participants []ParticipantState `json:"participants"`
}

func stateOf(r *room) ChannelState {
	st := ChannelState{ChannelID: r.channelID, Participants: make([]ParticipantState, 0, len(r.members))}
	for _, m := range r.members {
		st.Participants = append(st.Participants, ParticipantState{
			User: m.user, Muted: m.muted, Deafened: m.deafened,
			ServerMuted: m.serverMuted.Load(), CanSpeak: m.canSpeak.Load(),
		})
	}
	return st
}

// broadcastStateLocked tells everyone who can see the channel who is in it now. (An empty
// list = the channel is empty.)
func (s *Service) broadcastStateLocked(r *room) {
	view := r.view
	s.bc.BroadcastWhere("voice.state", stateOf(r), func(role perm.Role) bool { return role.AtLeast(view) })
}

// List returns the non-empty voice channels the viewer may see (GET /api/v1/voice).
func (s *Service) List(ctx context.Context, viewer accounts.User) []ChannelState {
	out := []ChannelState{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rooms {
		if viewer.Role.AtLeast(r.view) && len(r.members) > 0 {
			out = append(out, stateOf(r))
		}
	}
	return out
}

// findLocked returns the participant for a user in a channel, or nil.
func (s *Service) findLocked(channelID, userID int64) *participant {
	if r := s.rooms[channelID]; r != nil {
		for _, m := range r.members {
			if m.user.ID == userID {
				return m
			}
		}
	}
	return nil
}

// moderate checks the M5 hierarchy rule: the moderate_voice permission AND a role above
// the target's. The target's CURRENT role is used (it may have changed since joining).
func (s *Service) moderateLocked(by accounts.User, channelID, userID int64) (*participant, error) {
	target := s.findLocked(channelID, userID)
	if target == nil {
		return nil, ErrNotInVoice
	}
	if !perm.CanActOn(by.Role, perm.ModerateVoice, target.peer.User().Role) {
		return nil, accounts.ErrForbidden
	}
	return target, nil
}

// Disconnect removes someone from a voice channel (they can join again).
func (s *Service) Disconnect(by accounts.User, channelID, userID int64) error {
	if s == nil {
		return ErrUnavailable
	}
	defer s.recoverPanic("disconnect")
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := s.moderateLocked(by, channelID, userID)
	if err != nil {
		return err
	}
	s.leaveLocked(target, "disconnected")
	return nil
}

// SetServerMute mutes or unmutes someone for everyone. Their audio is no longer
// forwarded by the server, whatever their app does.
func (s *Service) SetServerMute(by accounts.User, channelID, userID int64, muted bool) error {
	if s == nil {
		return ErrUnavailable
	}
	defer s.recoverPanic("server mute")
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := s.moderateLocked(by, channelID, userID)
	if err != nil {
		return err
	}
	target.serverMuted.Store(muted)
	if muted && target.speaking {
		target.speaking = false
		ev := map[string]any{"channel_id": channelID, "user_id": userID, "speaking": false}
		for _, m := range target.room.members {
			m.peer.Send("voice.speaking", ev)
		}
	}
	s.broadcastStateLocked(target.room)
	return nil
}

// UserChanged re-checks a user's voice sessions after their role or name changed (call it
// after the hub knows the new role). Losing access ends the session.
func (s *Service) UserChanged(userID int64) {
	if s == nil {
		return
	}
	defer s.recoverPanic("user changed")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.byConn {
		if p.user.ID != userID {
			continue
		}
		u := p.peer.User()
		p.user, p.role = info(u), u.Role
		s.recheckLocked(p)
	}
}

// ChannelChanged re-reads a channel's rules (who may see / speak) and applies them.
func (s *Service) ChannelChanged(ctx context.Context, channelID int64) {
	if s == nil {
		return
	}
	defer s.recoverPanic("channel changed")
	rules, ok := s.channels(ctx, channelID)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rooms[channelID]
	if r == nil {
		return
	}
	if !ok || !rules.Voice {
		s.channelGoneLocked(r)
		return
	}
	oldView := r.view
	r.view, r.send = rules.View, rules.Send
	for _, p := range append([]*participant(nil), r.members...) {
		s.recheckLocked(p)
	}
	if rules.View.Above(oldView) {
		// People who could see the channel before but not now: for them it is empty.
		s.bc.BroadcastWhere("voice.state", ChannelState{ChannelID: channelID, Participants: []ParticipantState{}},
			func(role perm.Role) bool { return role.AtLeast(oldView) && !role.AtLeast(r.view) })
	}
}

// ChannelDeleted ends every session in a deleted channel.
func (s *Service) ChannelDeleted(channelID int64) {
	if s == nil {
		return
	}
	defer s.recoverPanic("channel deleted")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rooms[channelID]; r != nil {
		s.channelGoneLocked(r)
	}
}

func (s *Service) channelGoneLocked(r *room) {
	for _, p := range append([]*participant(nil), r.members...) {
		s.leaveLocked(p, "channel_deleted")
	}
}

// recheckLocked applies the room's rules to one participant.
func (s *Service) recheckLocked(p *participant) {
	r := p.room
	if !p.role.AtLeast(r.view) {
		s.leaveLocked(p, "no_access")
		return
	}
	can := p.role.AtLeast(r.send)
	if p.canSpeak.Swap(can) != can {
		p.peer.Send("voice.joined", map[string]any{"channel_id": r.channelID, "can_speak": can})
	}
	s.broadcastStateLocked(r)
}
