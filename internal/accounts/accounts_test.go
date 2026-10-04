package accounts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/5cfp/vianden-server/internal/auth"
	"github.com/5cfp/vianden-server/internal/perm"
	"github.com/5cfp/vianden-server/internal/testdb"
)

var ctx = context.Background()

// newService returns a service on an empty test database, plus its owner setup token.
func newService(t *testing.T) (*Service, string, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	svc, setupToken, err := NewService(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	return svc, setupToken, pool
}

// registerOwner creates the owner account and returns it.
func registerOwner(t *testing.T, svc *Service, setupToken string) User {
	t.Helper()
	res, err := svc.Register(ctx, RegisterInput{Username: "Osama", Password: "owner-password", InviteCode: setupToken})
	if err != nil {
		t.Fatalf("registering owner: %v", err)
	}
	return res.User
}

// createInvite inserts an invite directly (the invites API comes in a later step) and returns its code.
func createInvite(t *testing.T, pool *pgxpool.Pool, ownerID int64, maxUses int, expiresAt time.Time) string {
	t.Helper()
	code, hash := auth.NewToken(auth.InviteCodePrefix)
	_, err := pool.Exec(ctx, "INSERT INTO invites (code_hash, created_by, max_uses, expires_at) VALUES ($1, $2, $3, $4)",
		hash, ownerID, maxUses, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOwnerSetup(t *testing.T) {
	svc, setupToken, pool := newService(t)

	if !strings.HasPrefix(setupToken, "vo_") {
		t.Fatalf("setup token = %q, want vo_ prefix", setupToken)
	}

	res, err := svc.Register(ctx, RegisterInput{Username: "  Osama ", Password: "owner-password", InviteCode: setupToken})
	if err != nil {
		t.Fatal(err)
	}
	if !res.User.IsOwner() || res.User.Username != "osama" || res.User.DisplayName != "Osama" {
		t.Errorf("unexpected user: %+v", res.User)
	}

	// The session token works: its hash is in the database, and the token itself is not.
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE user_id = $1 AND token_hash = $2",
		res.User.ID, auth.HashToken(res.SessionToken)); n != 1 {
		t.Errorf("session rows with matching hash = %d, want 1", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE token_hash = $1", []byte(res.SessionToken)); n != 0 {
		t.Error("the plain session token was stored in the database")
	}

	// The password is stored as an Argon2id hash, never as plain text.
	var stored string
	pool.QueryRow(ctx, "SELECT password_hash FROM users WHERE id = $1", res.User.ID).Scan(&stored)
	if ok, _ := auth.VerifyPassword("owner-password", stored); !ok || strings.Contains(stored, "owner-password") {
		t.Errorf("password not stored as a proper hash: %q", stored)
	}
}

func TestSetupTokenWorksOnlyOnce(t *testing.T) {
	svc, setupToken, _ := newService(t)
	registerOwner(t, svc, setupToken)

	_, err := svc.Register(ctx, RegisterInput{Username: "second", Password: "password123", InviteCode: setupToken})
	if !errors.Is(err, ErrInvalidSetupToken) {
		t.Errorf("reused setup token: err = %v, want ErrInvalidSetupToken", err)
	}
}

func TestWrongSetupToken(t *testing.T) {
	svc, _, pool := newService(t)

	wrong, _ := auth.NewToken(auth.SetupTokenPrefix)
	_, err := svc.Register(ctx, RegisterInput{Username: "attacker", Password: "password123", InviteCode: wrong})
	if !errors.Is(err, ErrInvalidSetupToken) {
		t.Errorf("err = %v, want ErrInvalidSetupToken", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM users"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

func TestNoSetupTokenWhenOwnerExists(t *testing.T) {
	svc, setupToken, pool := newService(t)
	registerOwner(t, svc, setupToken)

	// Simulates a server restart: a new service must not create a new setup token.
	_, newToken, err := NewService(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if newToken != "" {
		t.Error("a setup token was created although the server already has an owner")
	}
}

func TestRegisterWithInvite(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken)
	code := createInvite(t, pool, owner.ID, 2, time.Now().Add(time.Hour))

	res, err := svc.Register(ctx, RegisterInput{Username: "friend", DisplayName: "My Friend", Password: "friend-password", InviteCode: code})
	if err != nil {
		t.Fatal(err)
	}
	if res.User.Role != perm.Member || res.User.DisplayName != "My Friend" {
		t.Errorf("unexpected user: %+v", res.User)
	}
	if n := count(t, pool, "SELECT uses FROM invites"); n != 1 {
		t.Errorf("invite uses = %d, want 1", n)
	}
}

func TestInviteUsedUp(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken)
	code := createInvite(t, pool, owner.ID, 1, time.Now().Add(time.Hour))

	if _, err := svc.Register(ctx, RegisterInput{Username: "first", Password: "password123", InviteCode: code}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Register(ctx, RegisterInput{Username: "second", Password: "password123", InviteCode: code})
	if !errors.Is(err, ErrInvalidInvite) {
		t.Errorf("err = %v, want ErrInvalidInvite", err)
	}
}

func TestExpiredAndUnknownInvites(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken)
	expired := createInvite(t, pool, owner.ID, 5, time.Now().Add(-time.Minute))
	unknown, _ := auth.NewToken(auth.InviteCodePrefix)

	for name, code := range map[string]string{"expired": expired, "unknown": unknown, "empty": "", "garbage": "hello"} {
		_, err := svc.Register(ctx, RegisterInput{Username: "user_" + name, Password: "password123", InviteCode: code})
		if !errors.Is(err, ErrInvalidInvite) {
			t.Errorf("%s invite: err = %v, want ErrInvalidInvite", name, err)
		}
	}
	if n := count(t, pool, "SELECT count(*) FROM users"); n != 1 {
		t.Errorf("users = %d, want only the owner", n)
	}
}

func TestUsernameTakenDoesNotUseInvite(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken) // username "osama"
	code := createInvite(t, pool, owner.ID, 1, time.Now().Add(time.Hour))

	_, err := svc.Register(ctx, RegisterInput{Username: "OSAMA", Password: "password123", InviteCode: code})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
	// The transaction rolled back, so the single-use invite is still usable.
	if n := count(t, pool, "SELECT uses FROM invites"); n != 0 {
		t.Errorf("invite uses = %d, want 0", n)
	}
	if _, err := svc.Register(ctx, RegisterInput{Username: "osama2", Password: "password123", InviteCode: code}); err != nil {
		t.Errorf("invite should still work: %v", err)
	}
}

func TestValidationHappensBeforeInviteCheck(t *testing.T) {
	svc, _, _ := newService(t)

	// Bad password with a bad invite: the validation error comes first, revealing nothing about invites.
	_, err := svc.Register(ctx, RegisterInput{Username: "someone", Password: "short", InviteCode: "vi_bogus"})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "password" {
		t.Errorf("err = %v, want a password ValidationError", err)
	}
}

// TestConcurrentUseOfSingleInvite checks the row lock: 10 people race for a 1-use invite.
func TestConcurrentUseOfSingleInvite(t *testing.T) {
	svc, setupToken, pool := newService(t)
	owner := registerOwner(t, svc, setupToken)
	code := createInvite(t, pool, owner.ID, 1, time.Now().Add(time.Hour))

	var wg sync.WaitGroup
	results := make(chan error, 10)
	for i := range 10 {
		wg.Go(func() {
			_, err := svc.Register(ctx, RegisterInput{Username: "racer" + string(rune('a'+i)), Password: "password123", InviteCode: code})
			results <- err
		})
	}
	wg.Wait()
	close(results)

	succeeded, rejected := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInvalidInvite):
			rejected++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 9 {
		t.Errorf("succeeded=%d rejected=%d, want 1 and 9", succeeded, rejected)
	}
	if n := count(t, pool, "SELECT uses FROM invites"); n != 1 {
		t.Errorf("invite uses = %d, want 1", n)
	}
}

// TestConcurrentOwnerSetup: two people racing with the same setup token, only one becomes owner.
func TestConcurrentOwnerSetup(t *testing.T) {
	svc, setupToken, pool := newService(t)

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range 5 {
		wg.Go(func() {
			_, errs[i] = svc.Register(ctx, RegisterInput{Username: "owner" + string(rune('a'+i)), Password: "password123", InviteCode: setupToken})
		})
	}
	wg.Wait()

	if n := count(t, pool, "SELECT count(*) FROM users WHERE role = 'owner'"); n != 1 {
		t.Errorf("owners = %d, want exactly 1", n)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrInvalidSetupToken) {
			t.Errorf("unexpected error: %v", err)
		}
	}
}
