package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
)

// fakeAccounts returns fixed results or errors, and remembers what it received.
type fakeAccounts struct {
	result accounts.AuthResult
	err    error
	got    *accounts.RegisterInput

	// For Authenticate: the one valid token, and the session it belongs to.
	validToken string
	session    accounts.Session
	authErr    error
	loggedOut  *int64 // receives the session ID passed to Logout

	// For the invite methods.
	invite    accounts.Invite
	invites   []accounts.Invite
	inviteErr error
	gotInvite *[3]int64 // receives (user ID, maxUses, hours)
	deleted   *int64

	// For member management.
	members    []accounts.Member
	memberErr  error
	memberCall *memberCall
}

func (f fakeAccounts) CreateInvite(_ context.Context, by accounts.User, maxUses, hours int) (accounts.Invite, string, error) {
	if f.inviteErr != nil {
		return accounts.Invite{}, "", f.inviteErr
	}
	if f.gotInvite != nil {
		*f.gotInvite = [3]int64{by.ID, int64(maxUses), int64(hours)}
	}
	return f.invite, "vi_newcode", nil
}

func (f fakeAccounts) ListInvites(_ context.Context, _ accounts.User) ([]accounts.Invite, error) {
	return f.invites, f.inviteErr
}

func (f fakeAccounts) DeleteInvite(_ context.Context, _ accounts.User, id int64) error {
	if f.deleted != nil {
		*f.deleted = id
	}
	return f.inviteErr
}

func (f fakeAccounts) Login(_ context.Context, _, _ string) (accounts.AuthResult, error) {
	return f.result, f.err
}

func (f fakeAccounts) Authenticate(_ context.Context, token string) (accounts.Session, error) {
	if f.authErr != nil {
		return accounts.Session{}, f.authErr
	}
	if f.validToken == "" || token != f.validToken {
		return accounts.Session{}, accounts.ErrUnauthenticated
	}
	return f.session, nil
}

func (f fakeAccounts) Logout(_ context.Context, sessionID int64) error {
	if f.loggedOut != nil {
		*f.loggedOut = sessionID
	}
	return nil
}

func (f fakeAccounts) Register(_ context.Context, in accounts.RegisterInput) (accounts.AuthResult, error) {
	if f.got != nil {
		*f.got = in
	}
	return f.result, f.err
}

func postRegister(t *testing.T, acc Accounts, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: acc, Chat: fakeChat{}, Logger: discardLogger})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/register", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func TestRegisterSuccess(t *testing.T) {
	var got accounts.RegisterInput
	acc := fakeAccounts{
		result: accounts.AuthResult{
			User:         accounts.User{ID: 7, Username: "osama", DisplayName: "Osama", Role: perm.Owner},
			SessionToken: "vs_secret",
		},
		got: &got,
	}

	rec := postRegister(t, acc, `{"username":"Osama","display_name":"Osama","password":"pw123456","invite_code":"vo_x"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	if got != (accounts.RegisterInput{Username: "Osama", DisplayName: "Osama", Password: "pw123456", InviteCode: "vo_x"}) {
		t.Errorf("service received %+v", got)
	}
	body := decode[sessionResponse](t, rec)
	want := userResponse{ID: 7, Username: "osama", DisplayName: "Osama", IsOwner: true, Role: "owner", Permissions: perm.Owner.Permissions()}
	if body.Token != "vs_secret" || !reflect.DeepEqual(body.User, want) {
		t.Errorf("unexpected response: %+v", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (response contains a token)", cc)
	}
}

func TestRegisterErrors(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"validation", &accounts.ValidationError{Field: "password", Message: "too short"}, 400, "invalid_password"},
		{"bad invite", accounts.ErrInvalidInvite, 403, "invalid_invite"},
		{"bad setup token", accounts.ErrInvalidSetupToken, 403, "invalid_setup_token"},
		{"username taken", accounts.ErrUsernameTaken, 409, "username_taken"},
		{"unexpected", errors.New("database exploded at 10.0.0.5"), 500, "internal_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postRegister(t, fakeAccounts{err: c.err}, `{"username":"a","password":"b","invite_code":"c"}`)
			if rec.Code != c.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, c.wantCode)
			}
			body := decode[errorResponse](t, rec)
			if body.Error.Code != c.wantErr {
				t.Errorf("error code = %q, want %q", body.Error.Code, c.wantErr)
			}
			if strings.Contains(body.Error.Message, "10.0.0.5") {
				t.Error("internal error details leaked to the client")
			}
		})
	}
}

func TestRegisterRejectsBadBodies(t *testing.T) {
	bodies := map[string]string{
		"not JSON":      `hello`,
		"unknown field": `{"username":"a","password":"b","invite_code":"c","is_owner":true}`,
		"two objects":   `{"username":"a"}{"username":"b"}`,
		"wrong type":    `{"username":123}`,
		"too large":     `{"username":"` + strings.Repeat("a", 70<<10) + `"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			rec := postRegister(t, fakeAccounts{}, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if got := decode[errorResponse](t, rec).Error.Code; got != "invalid_request" {
				t.Errorf("error code = %q, want invalid_request", got)
			}
		})
	}
}

func TestRegisterOnlyAcceptsPost(t *testing.T) {
	h := NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: fakeAccounts{}, Chat: fakeChat{}, Logger: discardLogger})
	rec := do(t, h, "GET", "/api/v1/register")
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		t.Errorf("GET /register returned %d", rec.Code)
	}
}

// ---- member management fakes (members_test.go) ----

func (f fakeAccounts) ListMembers(context.Context) ([]accounts.Member, error) {
	return f.members, f.memberErr
}

func (f fakeAccounts) SetRole(_ context.Context, by accounts.User, id int64, role perm.Role) (accounts.Member, error) {
	f.recordMember("role", by, id, string(role))
	if f.memberErr != nil {
		return accounts.Member{}, f.memberErr
	}
	return accounts.Member{User: accounts.User{ID: id, Username: "target", DisplayName: "Target", Role: role}}, nil
}

func (f fakeAccounts) Kick(_ context.Context, by accounts.User, id int64) error {
	f.recordMember("kick", by, id, "")
	return f.memberErr
}

func (f fakeAccounts) Ban(_ context.Context, by accounts.User, id int64, reason string) error {
	f.recordMember("ban", by, id, reason)
	return f.memberErr
}

func (f fakeAccounts) Unban(_ context.Context, by accounts.User, id int64) error {
	f.recordMember("unban", by, id, "")
	return f.memberErr
}

func (f fakeAccounts) recordMember(action string, by accounts.User, id int64, arg string) {
	if f.memberCall != nil {
		*f.memberCall = memberCall{action, by.ID, id, arg}
	}
}

type memberCall struct {
	action string
	byID   int64
	target int64
	arg    string
}
