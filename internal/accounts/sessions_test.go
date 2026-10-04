package accounts

import (
	"errors"
	"testing"
	"time"

	"github.com/5cfp/vianden-server/internal/auth"
)

func TestLoginAndAuthenticate(t *testing.T) {
	svc, setupToken, _ := newService(t)
	owner := registerOwner(t, svc, setupToken) // "Osama" / "owner-password"

	// Username is case-insensitive and trimmed, like at registration.
	res, err := svc.Login(ctx, "  OSAMA ", "owner-password")
	if err != nil {
		t.Fatal(err)
	}
	if res.User != owner {
		t.Errorf("logged in as %+v, want %+v", res.User, owner)
	}

	sess, err := svc.Authenticate(ctx, res.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	if sess.User != owner {
		t.Errorf("session user = %+v, want %+v", sess.User, owner)
	}
}

func TestEachLoginCreatesItsOwnSession(t *testing.T) {
	svc, setupToken, pool := newService(t)
	registerOwner(t, svc, setupToken) // registration = session 1

	a, _ := svc.Login(ctx, "osama", "owner-password") // e.g. desktop
	b, _ := svc.Login(ctx, "osama", "owner-password") // e.g. laptop
	if a.SessionToken == b.SessionToken {
		t.Error("two logins returned the same token")
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions"); n != 3 {
		t.Errorf("sessions = %d, want 3", n)
	}
}

func TestLoginFailuresLookTheSame(t *testing.T) {
	svc, setupToken, _ := newService(t)
	registerOwner(t, svc, setupToken)

	cases := map[string][2]string{
		"wrong password": {"osama", "wrong-password"},
		"unknown user":   {"nobody", "owner-password"},
		"empty":          {"", ""},
		"weird username": {"../../etc", "x"},
	}
	for name, c := range cases {
		_, err := svc.Login(ctx, c[0], c[1])
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", name, err)
		}
	}
}

func TestUnknownUserTakesAsLongAsWrongPassword(t *testing.T) {
	svc, setupToken, _ := newService(t)
	registerOwner(t, svc, setupToken)
	dummyHash() // compute the dummy hash once before timing

	measure := func(username string) time.Duration {
		start := time.Now()
		for range 5 {
			svc.Login(ctx, username, "wrong-password")
		}
		return time.Since(start)
	}
	known, unknown := measure("osama"), measure("nobody")

	// Without the dummy hash, "unknown" would be ~100x faster (no Argon2 at all).
	// Allow generous noise: just require the same order of magnitude.
	if unknown < known/3 {
		t.Errorf("unknown user: %v, known user: %v; the difference reveals which usernames exist", unknown, known)
	}
}

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	svc, setupToken, _ := newService(t)
	registerOwner(t, svc, setupToken)
	unknown, _ := auth.NewToken(auth.SessionTokenPrefix)

	for name, token := range map[string]string{
		"empty":        "",
		"unknown":      unknown,
		"wrong prefix": "vi_" + unknown[3:],
		"setup token":  setupToken,
		"very long":    "vs_" + string(make([]byte, 5000)),
	} {
		if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
	}
}

func TestLogoutRevokesOnlyThatSession(t *testing.T) {
	svc, setupToken, _ := newService(t)
	registerOwner(t, svc, setupToken)
	desktop, _ := svc.Login(ctx, "osama", "owner-password")
	laptop, _ := svc.Login(ctx, "osama", "owner-password")

	sess, _ := svc.Authenticate(ctx, desktop.SessionToken)
	if err := svc.Logout(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(ctx, desktop.SessionToken); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("logged-out token still works: err = %v", err)
	}
	if _, err := svc.Authenticate(ctx, laptop.SessionToken); err != nil {
		t.Errorf("other session stopped working: %v", err)
	}
	// Logging out twice is harmless.
	if err := svc.Logout(ctx, sess.ID); err != nil {
		t.Errorf("second logout: %v", err)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	svc, setupToken, pool := newService(t)
	res := registerAndLogin(t, svc, setupToken)

	pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1", auth.HashToken(res.SessionToken))

	if _, err := svc.Authenticate(ctx, res.SessionToken); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired token: err = %v, want ErrUnauthenticated", err)
	}
}

func TestSlidingExpiry(t *testing.T) {
	svc, setupToken, pool := newService(t)
	res := registerAndLogin(t, svc, setupToken)
	hash := auth.HashToken(res.SessionToken)

	// Pretend the session was last used 2 hours ago and expires in 1 day.
	pool.Exec(ctx, "UPDATE sessions SET last_used_at = now() - interval '2 hours', expires_at = now() + interval '1 day' WHERE token_hash = $1", hash)

	if _, err := svc.Authenticate(ctx, res.SessionToken); err != nil {
		t.Fatal(err)
	}

	var expires time.Time
	pool.QueryRow(ctx, "SELECT expires_at FROM sessions WHERE token_hash = $1", hash).Scan(&expires)
	if time.Until(expires) < SessionLifetime-time.Minute {
		t.Errorf("expiry not extended: expires in %v, want about %v", time.Until(expires), SessionLifetime)
	}
}

func TestRecentSessionIsNotRewritten(t *testing.T) {
	svc, setupToken, pool := newService(t)
	res := registerAndLogin(t, svc, setupToken)
	hash := auth.HashToken(res.SessionToken)

	var before, after time.Time
	pool.QueryRow(ctx, "SELECT last_used_at FROM sessions WHERE token_hash = $1", hash).Scan(&before)
	svc.Authenticate(ctx, res.SessionToken)
	pool.QueryRow(ctx, "SELECT last_used_at FROM sessions WHERE token_hash = $1", hash).Scan(&after)

	if !after.Equal(before) {
		t.Error("a session used less than an hour ago was written again (needless database write)")
	}
}

func registerAndLogin(t *testing.T, svc *Service, setupToken string) AuthResult {
	t.Helper()
	registerOwner(t, svc, setupToken)
	res, err := svc.Login(ctx, "osama", "owner-password")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDeleteOldSessions(t *testing.T) {
	svc, setupToken, pool := newService(t)
	registerOwner(t, svc, setupToken) // session 1: active
	loggedOut, _ := svc.Login(ctx, "osama", "owner-password")
	expired, _ := svc.Login(ctx, "osama", "owner-password")
	active, _ := svc.Login(ctx, "osama", "owner-password")

	s, _ := svc.Authenticate(ctx, loggedOut.SessionToken)
	svc.Logout(ctx, s.ID)
	pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE token_hash = $1", auth.HashToken(expired.SessionToken))

	n, err := svc.DeleteOldSessions(ctx)
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, err %v; want 2 (the logged-out and the expired one)", n, err)
	}
	if _, err := svc.Authenticate(ctx, active.SessionToken); err != nil {
		t.Errorf("an active session was deleted: %v", err)
	}
	if left := count(t, pool, "SELECT count(*) FROM sessions"); left != 2 {
		t.Errorf("sessions left = %d, want 2 active ones", left)
	}
}
