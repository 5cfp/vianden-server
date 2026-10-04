package accounts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/5cfp/vianden-server/internal/auth"
	"github.com/5cfp/vianden-server/internal/db"
)

// touchInterval: a session's expiry is pushed forward at most once per hour,
// so normal requests do not write to the database every time.
const touchInterval = time.Hour

var (
	// ErrInvalidCredentials is the ONLY login error: it never says whether the username
	// or the password was wrong, so attackers cannot find out which usernames exist.
	ErrInvalidCredentials = errors.New("wrong username or password")
	// ErrUnauthenticated means the session token is missing, unknown, expired, or revoked.
	ErrUnauthenticated = errors.New("missing, invalid, or expired session token")
)

// Session is a logged-in user's current session.
type Session struct {
	ID   int64
	User User
}

// dummyHash is checked when a username does not exist, so a login for an unknown user
// takes as long as a login with a wrong password. Without it, attackers could find
// existing usernames just by timing the responses (a "timing side channel").
var dummyHash = sync.OnceValue(func() string { return auth.HashPassword("dummy password for timing") })

// Login checks a username and password and starts a new session.
func (s *Service) Login(ctx context.Context, username, password string) (AuthResult, error) {
	u, err := s.queries.GetUserByUsername(ctx, NormalizeUsername(username))
	if errors.Is(err, pgx.ErrNoRows) {
		auth.VerifyPassword(password, dummyHash()) // same work as a real check, result ignored
		return AuthResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return AuthResult{}, err
	}

	ok, err := auth.VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return AuthResult{}, err // a broken hash in the database: a server problem, not a wrong password
	}
	if !ok {
		return AuthResult{}, ErrInvalidCredentials
	}

	token, err := createSession(ctx, s.queries, u.ID)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{User: toUser(u), SessionToken: token}, nil
}

// Authenticate checks a session token (as sent in the Authorization header) and returns its session.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	// Cheap checks first: obviously wrong tokens never reach the database.
	if !strings.HasPrefix(token, auth.SessionTokenPrefix) || len(token) > 100 {
		return Session{}, ErrUnauthenticated
	}

	row, err := s.queries.GetActiveSession(ctx, auth.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}

	if time.Since(row.Session.LastUsedAt) > touchInterval {
		err := s.queries.TouchSession(ctx, db.TouchSessionParams{ID: row.Session.ID, ExpiresAt: time.Now().Add(SessionLifetime)})
		if err != nil {
			return Session{}, err
		}
	}
	return Session{ID: row.Session.ID, User: toUser(row.User)}, nil
}

// Logout revokes a session. Its token never works again.
func (s *Service) Logout(ctx context.Context, sessionID int64) error {
	return s.queries.RevokeSession(ctx, sessionID)
}

func toUser(u db.User) User {
	return User{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, IsOwner: u.IsOwner}
}

// DeleteOldSessions removes expired and logged-out sessions; returns how many.
// They are useless (Authenticate already rejects them), so this only keeps the table small.
func (s *Service) DeleteOldSessions(ctx context.Context) (int64, error) {
	return s.queries.DeleteOldSessions(ctx)
}
