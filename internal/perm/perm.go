// Package perm is the ONE place that decides who may do what.
//
// Every user has exactly one Role. Roles are ranked (owner > admin > moderator > member),
// each role has a fixed set of permissions, and the hierarchy rule says you can only act
// on people BELOW you. Services ask this package; they never compare role names themselves,
// so a rule can never be "forgotten" in one place and enforced in another.
package perm

// Role is a user's role. The zero value "" is not a valid role.
type Role string

const (
	Owner     Role = "owner"
	Admin     Role = "admin"
	Moderator Role = "moderator"
	Member    Role = "member"
)

// AllRoles lists the roles from highest to lowest.
var AllRoles = []Role{Owner, Admin, Moderator, Member}

// Permission is one thing a role may do.
type Permission string

const (
	ManageChannels  Permission = "manage_channels"  // create, rename, delete channels; set who can see/write
	ManageInvites   Permission = "manage_invites"   // create, list, delete invite codes
	ManageRoles     Permission = "manage_roles"     // change other users' roles (below your own)
	DeleteMessages  Permission = "delete_messages"  // delete other people's messages
	KickMembers     Permission = "kick_members"     // sign a user out everywhere
	BanMembers      Permission = "ban_members"      // block a user from logging in (and unban)
	MentionEveryone Permission = "mention_everyone" // @everyone pings everyone who can see the channel (M6)
)

// AllPermissions lists every permission (used for the owner).
var AllPermissions = []Permission{ManageChannels, ManageInvites, ManageRoles, DeleteMessages, KickMembers, BanMembers, MentionEveryone}

// rolePermissions: what each role may do (decided in PROJECT_PLAN.md, M5).
var rolePermissions = map[Role][]Permission{
	Owner:     AllPermissions,
	Admin:     {ManageChannels, ManageInvites, ManageRoles, DeleteMessages, KickMembers, BanMembers, MentionEveryone},
	Moderator: {DeleteMessages, KickMembers, MentionEveryone},
	Member:    {},
}

// ParseRole turns a string from a request or the database into a Role.
func ParseRole(s string) (Role, bool) {
	r := Role(s)
	return r, r.Valid()
}

// Valid reports whether r is one of the four roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// rank: higher is more powerful. 0 for an invalid role (which can then do nothing).
func (r Role) rank() int {
	switch r {
	case Owner:
		return 4
	case Admin:
		return 3
	case Moderator:
		return 2
	case Member:
		return 1
	}
	return 0
}

// Has reports whether the role includes a permission.
func (r Role) Has(p Permission) bool {
	for _, have := range rolePermissions[r] {
		if have == p {
			return true
		}
	}
	return false
}

// Permissions lists the role's permissions (sent to clients so they can hide what the user cannot do).
func (r Role) Permissions() []Permission {
	out := make([]Permission, len(rolePermissions[r]))
	copy(out, rolePermissions[r])
	return out
}

// AtLeast reports whether r is min or higher (e.g. "who can see this channel").
func (r Role) AtLeast(min Role) bool { return r.Valid() && r.rank() >= min.rank() }

// Above reports whether r is strictly higher than other.
func (r Role) Above(other Role) bool { return r.rank() > other.rank() }

// CanActOn: the hierarchy rule for kick, ban and role changes. You need the permission
// AND the target must be strictly below you. So an admin cannot ban another admin or
// the owner, and nobody can act on the owner.
func CanActOn(actor Role, p Permission, target Role) bool {
	return actor.Has(p) && actor.Above(target)
}

// CanAssignRole: may actor change target's role to newRole? Needs ManageRoles, the target
// must be below the actor, and the NEW role must also be below the actor (you cannot make
// someone your equal or your superior). Nobody can make someone owner.
func CanAssignRole(actor, target, newRole Role) bool {
	return newRole.Valid() && newRole != Owner &&
		CanActOn(actor, ManageRoles, target) && actor.Above(newRole)
}
