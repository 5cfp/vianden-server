package accounts

import (
	"errors"
	"strings"
	"testing"

	"github.com/5cfp/vianden-server/internal/perm"
)

// team: an owner, an admin, a moderator, two members. Returns each as a User (+ their session tokens).
type team struct {
	svc                                *Service
	owner, admin, mod, member, member2 User
	tokens                             map[string]string
}

func newTeam(t *testing.T) team {
	t.Helper()
	svc, setupToken, _ := newService(t)
	tm := team{svc: svc, tokens: map[string]string{}}
	tm.owner = registerOwner(t, svc, setupToken)

	add := func(name string, role perm.Role) User {
		_, code, err := svc.CreateInvite(ctx, tm.owner, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Register(ctx, RegisterInput{Username: name, Password: name + "-password", InviteCode: code})
		if err != nil {
			t.Fatal(err)
		}
		tm.tokens[name] = res.SessionToken
		if role != perm.Member {
			m, err := svc.SetRole(ctx, tm.owner, res.User.ID, role)
			if err != nil {
				t.Fatal(err)
			}
			return m.User
		}
		return res.User
	}
	tm.admin = add("admin", perm.Admin)
	tm.mod = add("mod", perm.Moderator)
	tm.member = add("member", perm.Member)
	tm.member2 = add("member2", perm.Member)
	return tm
}

func TestListMembers(t *testing.T) {
	tm := newTeam(t)
	list, err := tm.svc.ListMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]perm.Role{}
	for _, m := range list {
		roles[m.Username] = m.Role
	}
	want := map[string]perm.Role{"osama": perm.Owner, "admin": perm.Admin, "mod": perm.Moderator, "member": perm.Member, "member2": perm.Member}
	for name, role := range want {
		if roles[name] != role {
			t.Errorf("%s has role %q, want %q", name, roles[name], role)
		}
	}
}

func TestRoleChangeHierarchy(t *testing.T) {
	tm := newTeam(t)
	cases := []struct {
		name    string
		by      User
		target  User
		newRole perm.Role
		wantErr error
	}{
		{"owner promotes member to admin", tm.owner, tm.member, perm.Admin, nil},
		{"admin promotes member to moderator", tm.admin, tm.member2, perm.Moderator, nil},
		{"admin cannot make an admin", tm.admin, tm.member2, perm.Admin, ErrForbidden},
		{"admin cannot demote the owner", tm.admin, tm.owner, perm.Member, ErrForbidden},
		{"moderator cannot change roles", tm.mod, tm.member2, perm.Moderator, ErrForbidden},
		{"member cannot promote themselves", tm.member2, tm.member2, perm.Admin, ErrForbidden},
		{"nobody can make an owner", tm.owner, tm.admin, perm.Owner, ErrForbidden},
		{"owner cannot demote themselves", tm.owner, tm.owner, perm.Admin, ErrForbidden},
	}
	for _, c := range cases {
		_, err := tm.svc.SetRole(ctx, c.by, c.target.ID, c.newRole)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
		}
	}

	if _, err := tm.svc.SetRole(ctx, tm.owner, tm.member.ID, perm.Role("root")); err == nil {
		t.Error("invalid role accepted")
	}
	if _, err := tm.svc.SetRole(ctx, tm.owner, 9999, perm.Member); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestRoleChangeTakesEffectImmediately(t *testing.T) {
	tm := newTeam(t)
	tm.svc.SetRole(ctx, tm.owner, tm.member.ID, perm.Admin)

	// The member's EXISTING session now carries the new role: no re-login needed, and a
	// demoted user loses their powers at once.
	s, err := tm.svc.Authenticate(ctx, tm.tokens["member"])
	if err != nil || s.User.Role != perm.Admin {
		t.Errorf("role in session = %q, err %v; want admin", s.User.Role, err)
	}
}

func TestKick(t *testing.T) {
	tm := newTeam(t)

	if err := tm.svc.Kick(ctx, tm.mod, tm.member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.svc.Authenticate(ctx, tm.tokens["member"]); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("kicked user's session still works: %v", err)
	}
	// A kick is not a ban: they can log in again.
	if _, err := tm.svc.Login(ctx, "member", "member-password"); err != nil {
		t.Errorf("kicked user cannot log in again: %v", err)
	}

	for name, c := range map[string]struct{ by, target User }{
		"moderator kicks moderator": {tm.mod, tm.mod},
		"moderator kicks admin":     {tm.mod, tm.admin},
		"admin kicks owner":         {tm.admin, tm.owner},
		"member kicks member":       {tm.member, tm.member2},
	} {
		if err := tm.svc.Kick(ctx, c.by, c.target.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: err = %v, want ErrForbidden", name, err)
		}
	}
}

func TestBanAndUnban(t *testing.T) {
	tm := newTeam(t)

	if err := tm.svc.Ban(ctx, tm.admin, tm.member.ID, "  spamming  "); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.svc.Authenticate(ctx, tm.tokens["member"]); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("banned user's session still works: %v", err)
	}

	_, err := tm.svc.Login(ctx, "member", "member-password")
	var banned *BannedError
	if !errors.As(err, &banned) || banned.Reason != "spamming" || !errors.Is(err, ErrBanned) {
		t.Fatalf("login of banned user: err = %v, want BannedError{spamming}", err)
	}
	// With a WRONG password, a banned account looks like any wrong login (reveals nothing).
	if _, err := tm.svc.Login(ctx, "member", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password on banned account: err = %v, want ErrInvalidCredentials", err)
	}

	list, _ := tm.svc.ListMembers(ctx)
	for _, m := range list {
		if m.Username == "member" && (m.BannedAt == nil || m.BanReason != "spamming") {
			t.Errorf("member list does not show the ban: %+v", m)
		}
	}

	if err := tm.svc.Unban(ctx, tm.admin, tm.member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.svc.Login(ctx, "member", "member-password"); err != nil {
		t.Errorf("unbanned user cannot log in: %v", err)
	}
}

func TestBanPermissions(t *testing.T) {
	tm := newTeam(t)
	for name, c := range map[string]struct{ by, target User }{
		"moderator bans member": {tm.mod, tm.member},
		"admin bans admin":      {tm.admin, tm.admin},
		"admin bans owner":      {tm.admin, tm.owner},
		"owner bans owner":      {tm.owner, tm.owner},
		"member bans member":    {tm.member, tm.member2},
	} {
		if err := tm.svc.Ban(ctx, c.by, c.target.ID, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: err = %v, want ErrForbidden", name, err)
		}
	}
	if err := tm.svc.Unban(ctx, tm.mod, tm.member.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("moderator unban: err = %v, want ErrForbidden", err)
	}
	if err := tm.svc.Ban(ctx, tm.owner, tm.admin.ID, ""); err != nil {
		t.Errorf("owner bans admin: %v", err)
	}
}

func TestBanReasonRules(t *testing.T) {
	tm := newTeam(t)
	for name, reason := range map[string]string{
		"too long":      strings.Repeat("x", 201),
		"rtl override":  "evil" + string(rune(0x202E)) + "txt",
		"control chars": "bad\x07reason",
	} {
		var ve *ValidationError
		if err := tm.svc.Ban(ctx, tm.owner, tm.member.ID, reason); !errors.As(err, &ve) || ve.Field != "reason" {
			t.Errorf("%s: err = %v, want reason ValidationError", name, err)
		}
	}
	if err := tm.svc.Ban(ctx, tm.owner, tm.member.ID, strings.Repeat("é", 200)); err != nil {
		t.Errorf("200-character reason should be allowed: %v", err)
	}
}

func TestUpdateDisplayName(t *testing.T) {
	tm := newTeam(t)
	m, err := tm.svc.UpdateDisplayName(ctx, tm.member.ID, "  Sara 🌙  ")
	if err != nil || m.DisplayName != "Sara 🌙" {
		t.Fatalf("got %q, %v; want trimmed name", m.DisplayName, err)
	}
	s, err := tm.svc.Authenticate(ctx, tm.tokens["member"])
	if err != nil || s.User.DisplayName != "Sara 🌙" {
		t.Errorf("session still shows %q (%v)", s.User.DisplayName, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("a", 33), "evil" + string(rune(0x202E))} {
		if _, err := tm.svc.UpdateDisplayName(ctx, tm.member.ID, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := tm.svc.UpdateDisplayName(ctx, 999999, "Ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}
