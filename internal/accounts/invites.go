package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/5cfp/vianden-server/internal/auth"
	"github.com/5cfp/vianden-server/internal/db"
	"github.com/5cfp/vianden-server/internal/perm"
)

// Invite limits (also documented in docs/API.md).
const (
	DefaultInviteUses  = 1
	MaxInviteUses      = 100
	DefaultInviteHours = 7 * 24  // 7 days
	MaxInviteHours     = 30 * 24 // 30 days
)

var (
	// ErrForbidden means the user is logged in but not allowed to do this.
	ErrForbidden = errors.New("you do not have permission to do this")
	// ErrInviteNotFound means no invite has the given ID.
	ErrInviteNotFound = errors.New("invite not found")
)

// Invite is an invite as shown to its managers. It never contains the code itself:
// only its hash is stored, so the code is visible exactly once, when it is created.
type Invite struct {
	ID        int64
	CreatedBy int64
	MaxUses   int
	Uses      int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// canManageInvites is the permission check (owner and admins, see the perm package).
// It is checked here, in the service, so no API endpoint can forget it.
func canManageInvites(u User) bool {
	return u.Role.Has(perm.ManageInvites)
}

// CreateInvite creates an invite code. maxUses 0 means the default (1); expiresInHours 0 means 7 days.
// It returns the invite and its code; the code cannot be retrieved again later.
func (s *Service) CreateInvite(ctx context.Context, by User, maxUses, expiresInHours int) (Invite, string, error) {
	if !canManageInvites(by) {
		return Invite{}, "", ErrForbidden
	}
	if maxUses == 0 {
		maxUses = DefaultInviteUses
	}
	if expiresInHours == 0 {
		expiresInHours = DefaultInviteHours
	}
	if maxUses < 1 || maxUses > MaxInviteUses {
		return Invite{}, "", &ValidationError{"max_uses", "must be between 1 and 100"}
	}
	// Checked as a plain number BEFORE converting to a duration: a huge value multiplied by
	// time.Hour would overflow and could wrap around into the "valid" range.
	if expiresInHours < 1 || expiresInHours > MaxInviteHours {
		return Invite{}, "", &ValidationError{"expires_in_hours", "must be between 1 and 720 (30 days)"}
	}

	code, hash := auth.NewToken(auth.InviteCodePrefix)
	inv, err := s.queries.CreateInvite(ctx, db.CreateInviteParams{
		CodeHash:  hash,
		CreatedBy: by.ID,
		MaxUses:   int32(maxUses),
		ExpiresAt: time.Now().Add(time.Duration(expiresInHours) * time.Hour),
	})
	if err != nil {
		return Invite{}, "", err
	}
	return toInvite(inv), code, nil
}

// ListInvites returns all invites, newest first (including used-up and expired ones).
func (s *Service) ListInvites(ctx context.Context, by User) ([]Invite, error) {
	if !canManageInvites(by) {
		return nil, ErrForbidden
	}
	rows, err := s.queries.ListInvites(ctx)
	if err != nil {
		return nil, err
	}
	invites := make([]Invite, len(rows))
	for i, r := range rows {
		invites[i] = toInvite(r)
	}
	return invites, nil
}

// DeleteInvite deletes an invite, so its code stops working (e.g. if it leaked).
// Accounts already created with it are not affected.
func (s *Service) DeleteInvite(ctx context.Context, by User, id int64) error {
	if !canManageInvites(by) {
		return ErrForbidden
	}
	n, err := s.queries.DeleteInvite(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInviteNotFound
	}
	return nil
}

func toInvite(i db.Invite) Invite {
	return Invite{
		ID:        i.ID,
		CreatedBy: i.CreatedBy,
		MaxUses:   int(i.MaxUses),
		Uses:      int(i.Uses),
		ExpiresAt: i.ExpiresAt,
		CreatedAt: i.CreatedAt,
	}
}
