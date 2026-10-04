package accounts

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/5cfp/vianden-server/internal/db"
	"github.com/5cfp/vianden-server/internal/perm"
)

// MaxBanReasonLength limits the ban reason (shown to the banned user and to admins).
const MaxBanReasonLength = 200

var (
	ErrUserNotFound = errors.New("user not found")
	// ErrBanned is returned by Login for a banned account (only after the password was correct,
	// so it reveals nothing to someone who does not know the password).
	ErrBanned = errors.New("this account is banned")
)

// Member is a user as seen in the member list.
type Member struct {
	User
	BannedAt  *time.Time // nil = not banned
	BanReason string
}

// ListMembers returns every account, sorted by username. Any logged-in user may see the
// member list; ban details are filtered by the API for users who cannot ban.
func (s *Service) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.queries.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	members := make([]Member, len(rows))
	for i, u := range rows {
		members[i] = toMember(u)
	}
	return members, nil
}

// SetRole changes target's role. Rules (perm.CanAssignRole): the actor needs manage_roles,
// the target must be below the actor, and so must the new role. Nobody can become owner.
func (s *Service) SetRole(ctx context.Context, by User, targetID int64, newRole perm.Role) (Member, error) {
	if !newRole.Valid() {
		return Member{}, &ValidationError{"role", "must be owner, admin, moderator, or member"}
	}
	target, err := s.getUser(ctx, targetID)
	if err != nil {
		return Member{}, err
	}
	if !perm.CanAssignRole(by.Role, target.Role, newRole) {
		return Member{}, ErrForbidden
	}
	u, err := s.queries.SetUserRole(ctx, db.SetUserRoleParams{ID: targetID, Role: string(newRole)})
	if err != nil {
		return Member{}, err
	}
	return toMember(u), nil
}

// Kick signs the target out on every device. They can log in again (it is a warning).
func (s *Service) Kick(ctx context.Context, by User, targetID int64) error {
	target, err := s.getUser(ctx, targetID)
	if err != nil {
		return err
	}
	if !perm.CanActOn(by.Role, perm.KickMembers, target.Role) {
		return ErrForbidden
	}
	_, err = s.queries.RevokeUserSessions(ctx, targetID)
	return err
}

// Ban blocks the target from logging in and signs them out everywhere, in one transaction.
// Their messages stay.
func (s *Service) Ban(ctx context.Context, by User, targetID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > MaxBanReasonLength || !utf8.ValidString(reason) {
		return &ValidationError{"reason", "must be at most 200 characters"}
	}
	if hasHiddenCharacters(reason) {
		return &ValidationError{"reason", "contains invisible or control characters"}
	}

	target, err := s.getUser(ctx, targetID)
	if err != nil {
		return err
	}
	if !perm.CanActOn(by.Role, perm.BanMembers, target.Role) {
		return ErrForbidden
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.BanUser(ctx, db.BanUserParams{ID: targetID, BanReason: reason}); err != nil {
			return err
		}
		_, err := q.RevokeUserSessions(ctx, targetID)
		return err
	})
}

// Unban lets the target log in again. Same rule as banning: the target must be below you.
func (s *Service) Unban(ctx context.Context, by User, targetID int64) error {
	target, err := s.getUser(ctx, targetID)
	if err != nil {
		return err
	}
	if !perm.CanActOn(by.Role, perm.BanMembers, target.Role) {
		return ErrForbidden
	}
	return s.queries.UnbanUser(ctx, targetID)
}

func (s *Service) getUser(ctx context.Context, id int64) (User, error) {
	u, err := s.queries.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	return toUser(u), nil
}

// withTx runs fn in a transaction: committed if it succeeds, rolled back otherwise.
func (s *Service) withTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func toMember(u db.User) Member {
	return Member{User: toUser(u), BannedAt: u.BannedAt, BanReason: u.BanReason}
}

// BannedError is ErrBanned with the reason the admin gave (shown to the banned user).
type BannedError struct{ Reason string }

func (e *BannedError) Error() string { return ErrBanned.Error() }

// Is makes errors.Is(err, ErrBanned) true for a *BannedError.
func (e *BannedError) Is(target error) bool { return target == ErrBanned }

// UpdateDisplayName changes a user's own display name (same rules as when registering).
func (s *Service) UpdateDisplayName(ctx context.Context, userID int64, name string) (Member, error) {
	name = strings.TrimSpace(name)
	if err := validateDisplayName(name); err != nil {
		return Member{}, err
	}
	u, err := s.queries.SetDisplayName(ctx, db.SetDisplayNameParams{ID: userID, DisplayName: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrUserNotFound
	}
	if err != nil {
		return Member{}, err
	}
	return toMember(u), nil
}

// GetMember returns one account (e.g. after its avatar changed).
func (s *Service) GetMember(ctx context.Context, id int64) (Member, error) {
	u, err := s.queries.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrUserNotFound
	}
	if err != nil {
		return Member{}, err
	}
	return toMember(u), nil
}
