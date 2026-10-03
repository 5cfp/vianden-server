package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
)

var expires = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func ownerSession() fakeAccounts {
	return fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
}

func TestCreateInvite(t *testing.T) {
	var got [3]int64
	acc := ownerSession()
	acc.invite = accounts.Invite{ID: 9, CreatedBy: 1, MaxUses: 3, ExpiresAt: expires, CreatedAt: expires.Add(-time.Hour)}
	acc.gotInvite = &got

	rec := send(t, handlerWith(acc), "POST", "/api/v1/invites", "Bearer vs_owner", `{"max_uses":3,"expires_in_hours":24}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	if got != [3]int64{1, 3, 24} {
		t.Errorf("service got (user, uses, hours) = %v, want [1 3 24]", got)
	}
	body := decode[createInviteResponse](t, rec)
	if body.Code != "vi_newcode" || body.Invite.ID != 9 || !body.Invite.ExpiresAt.Equal(expires) {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestCreateInviteErrors(t *testing.T) {
	cases := []struct {
		err      error
		wantCode int
		wantErr  string
	}{
		{accounts.ErrForbidden, 403, "forbidden"},
		{&accounts.ValidationError{Field: "max_uses", Message: "too many"}, 400, "invalid_max_uses"},
	}
	for _, c := range cases {
		acc := ownerSession()
		acc.inviteErr = c.err
		rec := send(t, handlerWith(acc), "POST", "/api/v1/invites", "Bearer vs_owner", `{}`)
		if rec.Code != c.wantCode || decode[errorResponse](t, rec).Error.Code != c.wantErr {
			t.Errorf("%v: status %d, want %d %s", c.err, rec.Code, c.wantCode, c.wantErr)
		}
	}
}

func TestInviteEndpointsNeedLogin(t *testing.T) {
	h := handlerWith(ownerSession())
	for _, r := range [][2]string{{"POST", "/api/v1/invites"}, {"GET", "/api/v1/invites"}, {"DELETE", "/api/v1/invites/1"}} {
		if rec := send(t, h, r[0], r[1], "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: status %d, want 401", r[0], r[1], rec.Code)
		}
	}
}

func TestListInvites(t *testing.T) {
	acc := ownerSession()
	rec := send(t, handlerWith(acc), "GET", "/api/v1/invites", "Bearer vs_owner", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"invites\":[]}\n" {
		t.Errorf("empty list: status %d body %q, want [] (not null)", rec.Code, rec.Body)
	}

	acc.invites = []accounts.Invite{{ID: 2, MaxUses: 1, ExpiresAt: expires}, {ID: 1, MaxUses: 5, Uses: 2, ExpiresAt: expires}}
	rec = send(t, handlerWith(acc), "GET", "/api/v1/invites", "Bearer vs_owner", "")
	body := decode[listInvitesResponse](t, rec)
	if len(body.Invites) != 2 || body.Invites[1].Uses != 2 {
		t.Errorf("unexpected list: %+v", body)
	}
}

func TestDeleteInvite(t *testing.T) {
	var deleted int64
	acc := ownerSession()
	acc.deleted = &deleted

	rec := send(t, handlerWith(acc), "DELETE", "/api/v1/invites/42", "Bearer vs_owner", "")
	if rec.Code != http.StatusNoContent || deleted != 42 {
		t.Errorf("status %d, deleted %d; want 204 and 42", rec.Code, deleted)
	}

	for _, bad := range []string{"abc", "-", "99999999999999999999"} {
		if rec := send(t, handlerWith(acc), "DELETE", "/api/v1/invites/"+bad, "Bearer vs_owner", ""); rec.Code != http.StatusNotFound {
			t.Errorf("id %q: status %d, want 404", bad, rec.Code)
		}
	}

	acc.inviteErr = accounts.ErrInviteNotFound
	if rec := send(t, handlerWith(acc), "DELETE", "/api/v1/invites/7", "Bearer vs_owner", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown invite: status %d, want 404", rec.Code)
	}
}
