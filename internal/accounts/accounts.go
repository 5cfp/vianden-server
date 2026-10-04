// Package accounts handles user accounts: owner setup, registration, and sessions.
package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/5cfp/vianden-server/internal/auth"
	"github.com/5cfp/vianden-server/internal/db"
	"github.com/5cfp/vianden-server/internal/perm"
)

// SessionLifetime is how long a session stays valid without being used.
const SessionLifetime = 30 * 24 * time.Hour

// Errors returned by Register. Their messages are safe to show to the user.
var (
	ErrInvalidInvite     = errors.New("invite code is invalid, expired, or used up")
	ErrInvalidSetupToken = errors.New("setup token is invalid, or the server already has an owner")
	ErrUsernameTaken     = errors.New("username is already taken")
)

// Service contains the account logic. It is safe to use from many goroutines.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries

	mu sync.Mutex
	// setupTokenHash is the SHA-256 of the one-time owner setup token.
	// It lives only in memory: nil once the server has an owner.
	setupTokenHash []byte
}

// NewService creates the account service. If the server has no owner yet, it also returns
// a new one-time setup token, which the caller prints to the console. Otherwise setupToken is "".
func NewService(ctx context.Context, pool *pgxpool.Pool) (svc *Service, setupToken string, err error) {
	svc = &Service{pool: pool, queries: db.New(pool)}

	hasOwner, err := svc.queries.OwnerExists(ctx)
	if err != nil {
		return nil, "", err
	}
	if !hasOwner {
		setupToken, svc.setupTokenHash = auth.NewToken(auth.SetupTokenPrefix)
	}
	return svc, setupToken, nil
}

// RegisterInput is what a new user sends. InviteCode is either an invite code ("vi_...")
// or, for the very first account, the owner setup token ("vo_...").
type RegisterInput struct {
	Username    string
	DisplayName string // optional: defaults to the username as typed
	Password    string
	InviteCode  string
}

// User is the public view of an account (no password hash).
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Role        perm.Role
}

// IsOwner reports whether the user is the server owner.
func (u User) IsOwner() bool { return u.Role == perm.Owner }

// AuthResult is an account plus a new session token (returned by Register and Login).
type AuthResult struct {
	User         User
	SessionToken string
}

// Register creates an account. It returns a *ValidationError for bad input, or one of the Err* values.
func (s *Service) Register(ctx context.Context, in RegisterInput) (AuthResult, error) {
	username := NormalizeUsername(in.Username)
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(in.Username)
	}

	// Check the input first. Nothing about invites or existing users is revealed by these errors.
	if err := validateUsername(username); err != nil {
		return AuthResult{}, err
	}
	if err := validateDisplayName(displayName); err != nil {
		return AuthResult{}, err
	}
	if err := validatePassword(in.Password); err != nil {
		return AuthResult{}, err
	}

	code := strings.TrimSpace(in.InviteCode)
	// New accounts are members; registerOwner raises the first one to owner.
	user := db.CreateUserParams{Username: username, DisplayName: displayName, Role: string(perm.Member)}

	if strings.HasPrefix(code, auth.SetupTokenPrefix) {
		return s.registerOwner(ctx, code, user, in.Password)
	}
	return s.registerWithInvite(ctx, code, user, in.Password)
}

func (s *Service) registerOwner(ctx context.Context, setupToken string, user db.CreateUserParams, password string) (AuthResult, error) {
	if !s.checkSetupToken(setupToken) {
		return AuthResult{}, ErrInvalidSetupToken
	}

	user.PasswordHash = auth.HashPassword(password)
	user.Role = string(perm.Owner)

	result, err := s.inTx(ctx, func(q *db.Queries) (AuthResult, error) {
		return s.createUserWithSession(ctx, q, user)
	})
	if err != nil {
		return AuthResult{}, err
	}

	// The token has done its job: forget it so it can never be used again.
	s.mu.Lock()
	s.setupTokenHash = nil
	s.mu.Unlock()
	return result, nil
}

func (s *Service) registerWithInvite(ctx context.Context, code string, user db.CreateUserParams, password string) (AuthResult, error) {
	// Everything below happens in ONE transaction: either the account is created AND the
	// invite use is counted AND the session is created, or nothing happens at all.
	return s.inTx(ctx, func(q *db.Queries) (AuthResult, error) {
		invite, err := q.LockUsableInvite(ctx, auth.HashToken(code))
		if errors.Is(err, pgx.ErrNoRows) {
			return AuthResult{}, ErrInvalidInvite
		}
		if err != nil {
			return AuthResult{}, err
		}

		// Hashed only after the invite is confirmed valid, so random requests without a
		// valid invite cannot make the server do the expensive Argon2id work.
		user.PasswordHash = auth.HashPassword(password)

		result, err := s.createUserWithSession(ctx, q, user)
		if err != nil {
			return AuthResult{}, err // e.g. username taken: the rollback also "un-uses" the invite
		}
		if err := q.UseInvite(ctx, invite.ID); err != nil {
			return AuthResult{}, err
		}
		return result, nil
	})
}

func (s *Service) createUserWithSession(ctx context.Context, q *db.Queries, params db.CreateUserParams) (AuthResult, error) {
	u, err := q.CreateUser(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			switch pgErr.ConstraintName {
			case "users_username_key":
				return AuthResult{}, ErrUsernameTaken
			case "users_single_owner_idx": // another owner registered at the same moment
				return AuthResult{}, ErrInvalidSetupToken
			}
		}
		return AuthResult{}, err
	}

	token, err := createSession(ctx, q, u.ID)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{
		User:         toUser(u),
		SessionToken: token,
	}, nil
}

// createSession stores a new session and returns its token. Only the token's hash is stored.
func createSession(ctx context.Context, q *db.Queries, userID int64) (string, error) {
	token, hash := auth.NewToken(auth.SessionTokenPrefix)
	err := q.CreateSession(ctx, db.CreateSessionParams{
		UserID:    userID,
		TokenHash: hash,
		ExpiresAt: time.Now().Add(SessionLifetime),
	})
	return token, err
}

// checkSetupToken compares in constant time (see auth.VerifyPassword for why).
func (s *Service) checkSetupToken(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupTokenHash == nil {
		return false
	}
	return subtle.ConstantTimeCompare(auth.HashToken(token), s.setupTokenHash) == 1
}

// inTx runs fn inside a database transaction: committed if fn succeeds, rolled back if it fails.
func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries) (AuthResult, error)) (AuthResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AuthResult{}, err
	}
	defer tx.Rollback(ctx) // does nothing if Commit already succeeded

	result, err := fn(s.queries.WithTx(tx))
	if err != nil {
		return AuthResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AuthResult{}, err
	}
	return result, nil
}
