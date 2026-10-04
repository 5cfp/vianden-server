package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
)

var osama = accounts.User{ID: 1, Username: "osama", DisplayName: "Osama", IsOwner: true}

// send makes a request with an optional bearer token and JSON body.
func send(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func handlerWith(acc fakeAccounts) http.Handler {
	return NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: acc, Chat: fakeChat{}, Logger: discardLogger})
}

func TestLoginSuccess(t *testing.T) {
	h := handlerWith(fakeAccounts{result: accounts.AuthResult{User: osama, SessionToken: "vs_abc"}})
	rec := send(t, h, "POST", "/api/v1/login", "", `{"username":"osama","password":"pw"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := decode[sessionResponse](t, rec); body.Token != "vs_abc" || body.User.Username != "osama" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestLoginErrors(t *testing.T) {
	rec := send(t, handlerWith(fakeAccounts{err: accounts.ErrInvalidCredentials}), "POST", "/api/v1/login", "", `{"username":"x","password":"y"}`)
	if rec.Code != http.StatusUnauthorized || decode[errorResponse](t, rec).Error.Code != "invalid_credentials" {
		t.Errorf("wrong credentials: status %d", rec.Code)
	}

	rec = send(t, handlerWith(fakeAccounts{err: errors.New("db down at 10.0.0.5")}), "POST", "/api/v1/login", "", `{"username":"x","password":"y"}`)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("internal error: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestMeWithValidToken(t *testing.T) {
	h := handlerWith(fakeAccounts{validToken: "vs_good", session: accounts.Session{ID: 5, User: osama}})

	for _, header := range []string{"Bearer vs_good", "bearer vs_good", "BEARER  vs_good"} {
		rec := send(t, h, "GET", "/api/v1/me", header, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%q: status = %d, want 200", header, rec.Code)
			continue
		}
		if got := decode[meResponse](t, rec).User; got != toUserResponse(osama) {
			t.Errorf("%q: user = %+v", header, got)
		}
	}
}

func TestMeRejectsMissingOrBadTokens(t *testing.T) {
	h := handlerWith(fakeAccounts{validToken: "vs_good", session: accounts.Session{ID: 5, User: osama}})

	for _, header := range []string{"", "vs_good", "Basic vs_good", "Bearer", "Bearer ", "Bearer vs_wrong", "Token vs_good"} {
		rec := send(t, h, "GET", "/api/v1/me", header, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", header, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: missing WWW-Authenticate header", header)
		}
		if code := decode[errorResponse](t, rec).Error.Code; code != "unauthorized" {
			t.Errorf("%q: error code = %q", header, code)
		}
	}
}

func TestAuthDatabaseErrorIs500NotLogout(t *testing.T) {
	// If the database is down, the client must NOT be told its token is invalid
	// (it would throw the token away and log the user out for no reason).
	h := handlerWith(fakeAccounts{authErr: errors.New("connection refused")})
	rec := send(t, h, "GET", "/api/v1/me", "Bearer vs_good", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestLogout(t *testing.T) {
	var loggedOut int64
	h := handlerWith(fakeAccounts{validToken: "vs_good", session: accounts.Session{ID: 42, User: osama}, loggedOut: &loggedOut})

	rec := send(t, h, "POST", "/api/v1/logout", "Bearer vs_good", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if loggedOut != 42 {
		t.Errorf("revoked session %d, want 42 (the caller's own session)", loggedOut)
	}

	if rec := send(t, h, "POST", "/api/v1/logout", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("logout without token: status = %d, want 401", rec.Code)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	h := handlerWith(fakeAccounts{err: accounts.ErrInvalidCredentials})

	for i := range 10 {
		if rec := send(t, h, "POST", "/api/v1/login", "", `{"username":"x","password":"y"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	rec := send(t, h, "POST", "/api/v1/login", "", `{"username":"x","password":"y"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}

	// Register shares the same limit, so switching endpoints does not help an attacker.
	if rec := send(t, h, "POST", "/api/v1/register", "", `{}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("register after login limit: status = %d, want 429", rec.Code)
	}
}
