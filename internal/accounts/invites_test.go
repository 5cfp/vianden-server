package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/5cfp/vianden-server/internal/perm"
)

func TestOwnerCreatesInviteThatWorks(t *testing.T) {
	svc, setupToken, _ := newService(t)
	owner := registerOwner(t, svc, setupToken)

	inv, code, err := svc.CreateInvite(ctx, owner, 0, 0) // defaults
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "vi_") || inv.MaxUses != 1 || inv.Uses != 0 || inv.CreatedBy != owner.ID {
		t.Errorf("unexpected invite %+v / code %q", inv, code)
	}
	if d := time.Until(inv.ExpiresAt); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour {
		t.Errorf("default expiry in %v, want 7 days", d)
	}

	if _, err := svc.Register(ctx, RegisterInput{Username: "friend", Password: "password123", InviteCode: code}); err != nil {
		t.Fatalf("registering with the new invite: %v", err)
	}
}

func TestInviteSettings(t *testing.T) {
	svc, setupToken, _ := newService(t)
	owner := registerOwner(t, svc, setupToken)

	inv, _, err := svc.CreateInvite(ctx, owner, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if inv.MaxUses != 5 {
		t.Errorf("max uses = %d, want 5", inv.MaxUses)
	}
	if d := time.Until(inv.ExpiresAt); d < 2*time.Hour-time.Minute || d > 2*time.Hour {
		t.Errorf("expiry in %v, want 2h", d)
	}
}

func TestInviteLimits(t *testing.T) {
	svc, setupToken, _ := newService(t)
	owner := registerOwner(t, svc, setupToken)

	cases := []struct {
		maxUses, hours int
		field          string
	}{
		{-1, 0, "max_uses"},
		{101, 0, "max_uses"},
		{0, -5, "expires_in_hours"},
		{0, 721, "expires_in_hours"},
		{0, 9_000_000_000_000, "expires_in_hours"}, // would overflow time.Duration
	}
	for _, c := range cases {
		_, _, err := svc.CreateInvite(ctx, owner, c.maxUses, c.hours)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != c.field {
			t.Errorf("CreateInvite(%d, %d): err = %v, want %s ValidationError", c.maxUses, c.hours, err, c.field)
		}
	}
	// The edges are allowed.
	if _, _, err := svc.CreateInvite(ctx, owner, 100, 720); err != nil {
		t.Errorf("100 uses / 720 hours should be allowed: %v", err)
	}
	if _, _, err := svc.CreateInvite(ctx, owner, 1, 1); err != nil {
		t.Errorf("1 use / 1 hour should be allowed: %v", err)
	}
}

func TestMembersCannotManageInvites(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken)
	_, code, _ := svc.CreateInvite(ctx, owner, 1, 0)
	res, err := svc.Register(ctx, RegisterInput{Username: "member", Password: "password123", InviteCode: code})
	if err != nil {
		t.Fatal(err)
	}
	member := res.User

	if _, _, err := svc.CreateInvite(ctx, member, 1, 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("member create: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.ListInvites(ctx, member); !errors.Is(err, ErrForbidden) {
		t.Errorf("member list: err = %v, want ErrForbidden", err)
	}
	if err := svc.DeleteInvite(ctx, member, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("member delete: err = %v, want ErrForbidden", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM invites"); n != 1 {
		t.Errorf("invites = %d, want 1: the member must not create or delete any", n)
	}
}

func TestListAndDeleteInvites(t *testing.T) {
	svc, setupToken, _ := newService(t)
	owner := registerOwner(t, svc, setupToken)
	first, _, _ := svc.CreateInvite(ctx, owner, 1, 0)
	second, code, _ := svc.CreateInvite(ctx, owner, 3, 0)

	list, err := svc.ListInvites(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("list = %+v, want [second, first] (newest first)", list)
	}

	// Deleting an invite makes its code stop working.
	if err := svc.DeleteInvite(ctx, owner, second.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Register(ctx, RegisterInput{Username: "late", Password: "password123", InviteCode: code})
	if !errors.Is(err, ErrInvalidInvite) {
		t.Errorf("deleted invite: err = %v, want ErrInvalidInvite", err)
	}

	if err := svc.DeleteInvite(ctx, owner, second.ID); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrInviteNotFound", err)
	}
}

func TestDeletingUsedInviteKeepsAccounts(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken)
	inv, code, _ := svc.CreateInvite(ctx, owner, 1, 0)
	if _, err := svc.Register(ctx, RegisterInput{Username: "friend", Password: "password123", InviteCode: code}); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteInvite(ctx, owner, inv.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM users WHERE username = 'friend'"); n != 1 {
		t.Error("deleting an invite removed the account created with it")
	}
}

func TestInviteManagementFollowsRoles(t *testing.T) {
	svc, setupToken, _ := newService(t)
	registerOwner(t, svc, setupToken)
	for role, allowed := range map[perm.Role]bool{perm.Admin: true, perm.Moderator: false, perm.Member: false} {
		u := User{ID: 1, Username: "x", Role: role} // only the role matters for the check
		_, _, err := svc.CreateInvite(ctx, u, 1, 1)
		if allowed && err != nil {
			t.Errorf("%s should manage invites: %v", role, err)
		}
		if !allowed && !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: err = %v, want ErrForbidden", role, err)
		}
	}
}
