package perm

import "testing"

func TestParseRole(t *testing.T) {
	for _, s := range []string{"owner", "admin", "moderator", "member"} {
		if r, ok := ParseRole(s); !ok || string(r) != s {
			t.Errorf("ParseRole(%q) = %q, %v", s, r, ok)
		}
	}
	for _, s := range []string{"", "Owner", "superuser", "admin "} {
		if _, ok := ParseRole(s); ok {
			t.Errorf("ParseRole(%q) accepted", s)
		}
	}
}

func TestPermissionTable(t *testing.T) {
	// The table from PROJECT_PLAN.md (M5, + mention_everyone in M6, + moderate_voice in M7). If you change a role's permissions, change both.
	want := map[Role]map[Permission]bool{
		Owner:     {ManageChannels: true, ManageInvites: true, ManageRoles: true, DeleteMessages: true, KickMembers: true, BanMembers: true, MentionEveryone: true, ModerateVoice: true},
		Admin:     {ManageChannels: true, ManageInvites: true, ManageRoles: true, DeleteMessages: true, KickMembers: true, BanMembers: true, MentionEveryone: true, ModerateVoice: true},
		Moderator: {DeleteMessages: true, KickMembers: true, MentionEveryone: true, ModerateVoice: true},
		Member:    {},
	}
	for role, perms := range want {
		for _, p := range AllPermissions {
			if got := role.Has(p); got != perms[p] {
				t.Errorf("%s has %s = %v, want %v", role, p, got, perms[p])
			}
		}
	}
	if Role("").Has(DeleteMessages) || Role("hacker").Has(ManageChannels) {
		t.Error("an invalid role must have no permissions")
	}
}

func TestPermissionsReturnsACopy(t *testing.T) {
	p := Admin.Permissions()
	p[0] = "hacked"
	if Admin.Permissions()[0] == "hacked" {
		t.Error("callers could change the permission table through the returned slice")
	}
}

func TestHierarchy(t *testing.T) {
	cases := []struct {
		actor  Role
		p      Permission
		target Role
		want   bool
	}{
		{Owner, BanMembers, Admin, true},
		{Admin, BanMembers, Moderator, true},
		{Admin, BanMembers, Member, true},
		{Admin, BanMembers, Admin, false}, // not your equal
		{Admin, BanMembers, Owner, false}, // never the owner
		{Moderator, KickMembers, Member, true},
		{Moderator, KickMembers, Moderator, false},
		{Moderator, BanMembers, Member, false}, // moderators cannot ban
		{Member, KickMembers, Member, false},
		{Owner, KickMembers, Owner, false}, // not even the owner on themselves
		{Role("bogus"), KickMembers, Member, false},
	}
	for _, c := range cases {
		if got := CanActOn(c.actor, c.p, c.target); got != c.want {
			t.Errorf("CanActOn(%s, %s, %s) = %v, want %v", c.actor, c.p, c.target, got, c.want)
		}
	}
}

func TestCanAssignRole(t *testing.T) {
	cases := []struct {
		actor, target, newRole Role
		want                   bool
	}{
		{Owner, Member, Admin, true},
		{Owner, Admin, Member, true},
		{Owner, Member, Owner, false}, // nobody can create a second owner
		{Admin, Member, Moderator, true},
		{Admin, Moderator, Member, true},
		{Admin, Member, Admin, false},      // cannot make someone your equal
		{Admin, Admin, Member, false},      // cannot demote your equal
		{Admin, Owner, Member, false},      // cannot touch the owner
		{Moderator, Member, Member, false}, // moderators cannot change roles
		{Owner, Member, Role("root"), false},
	}
	for _, c := range cases {
		if got := CanAssignRole(c.actor, c.target, c.newRole); got != c.want {
			t.Errorf("CanAssignRole(%s, %s -> %s) = %v, want %v", c.actor, c.target, c.newRole, got, c.want)
		}
	}
}

func TestAtLeast(t *testing.T) {
	if !Admin.AtLeast(Moderator) || !Moderator.AtLeast(Moderator) || Member.AtLeast(Moderator) {
		t.Error("AtLeast ordering is wrong")
	}
	if Role("").AtLeast(Member) {
		t.Error("an invalid role must not pass any minimum")
	}
}
