package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
)

var bannedAt = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func membersFake() fakeAccounts {
	acc := ownerSession() // "vs_owner" = osama (owner)
	acc.members = []accounts.Member{
		{User: accounts.User{ID: 1, Username: "osama", DisplayName: "Osama", Role: perm.Owner}},
		{User: accounts.User{ID: 2, Username: "spammer", DisplayName: "Spammer", Role: perm.Member}, BannedAt: &bannedAt, BanReason: "spam"},
	}
	return acc
}

func TestMemberListHidesBanDetailsFromNonBanners(t *testing.T) {
	acc := membersFake()
	h := NewHandler(Deps{ServerName: "T", DB: healthyDB, Accounts: memberToo{acc}, Chat: fakeChat{}, Logger: discardLogger})

	// The owner (may ban) sees ban details.
	owner := send(t, h, "GET", "/api/v1/users", "Bearer vs_owner", "").Body.String()
	if !strings.Contains(owner, `"ban_reason":"spam"`) || !strings.Contains(owner, `"banned":true`) {
		t.Errorf("owner should see ban details: %s", owner)
	}

	// A member ("vs_member", see memberToo) does not.
	memberView := send(t, h, "GET", "/api/v1/users", "Bearer vs_member", "").Body.String()
	if strings.Contains(memberView, "ban") {
		t.Errorf("member must not see ban details: %s", memberView)
	}
	if !strings.Contains(memberView, `"role":"owner"`) {
		t.Errorf("roles should be visible to everyone: %s", memberView)
	}
}

func TestSetRoleEndpoint(t *testing.T) {
	var call memberCall
	rt := &fakeRealtime{}
	acc := membersFake()
	acc.memberCall = &call
	h := NewHandler(Deps{ServerName: "T", DB: healthyDB, Accounts: acc, Chat: fakeChat{}, Realtime: rt, Logger: discardLogger})

	rec := send(t, h, "PATCH", "/api/v1/users/2", "Bearer vs_owner", `{"role":"moderator"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if call != (memberCall{"role", 1, 2, "moderator"}) {
		t.Errorf("service call %+v", call)
	}
	if len(rt.events) != 1 || rt.events[0] != "member.updated" {
		t.Errorf("events %v, want member.updated", rt.events)
	}
	if ev := rt.data[0].(memberResponse); ev.Banned != nil {
		t.Error("the broadcast to everyone must not include ban details")
	}
}

func TestKickAndBanEndConnections(t *testing.T) {
	for _, c := range []struct{ method, path, body, action string }{
		{"POST", "/api/v1/users/2/kick", "", "kick"},
		{"POST", "/api/v1/users/2/ban", `{"reason":"spam"}`, "ban"},
	} {
		var call memberCall
		rt := &fakeRealtime{}
		acc := membersFake()
		acc.memberCall = &call
		h := NewHandler(Deps{ServerName: "T", DB: healthyDB, Accounts: acc, Chat: fakeChat{}, Realtime: rt, Logger: discardLogger})

		rec := send(t, h, c.method, c.path, "Bearer vs_owner", c.body)
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: status %d", c.action, rec.Code)
		}
		if call.action != c.action || call.target != 2 {
			t.Errorf("%s: call %+v", c.action, call)
		}
		if len(rt.endedUsers) != 1 || rt.endedUsers[0] != 2 {
			t.Errorf("%s: live connections of user 2 not closed (%v)", c.action, rt.endedUsers)
		}
	}
}

func TestMemberErrors(t *testing.T) {
	cases := []struct {
		err      error
		wantCode int
		wantErr  string
	}{
		{accounts.ErrForbidden, 403, "forbidden"},
		{accounts.ErrUserNotFound, 404, "not_found"},
		{&accounts.ValidationError{Field: "role", Message: "bad"}, 400, "invalid_role"},
		{&accounts.ValidationError{Field: "reason", Message: "bad"}, 400, "invalid_reason"},
	}
	for _, c := range cases {
		rt := &fakeRealtime{}
		acc := membersFake()
		acc.memberErr = c.err
		h := NewHandler(Deps{ServerName: "T", DB: healthyDB, Accounts: acc, Chat: fakeChat{}, Realtime: rt, Logger: discardLogger})
		rec := send(t, h, "POST", "/api/v1/users/2/ban", "Bearer vs_owner", `{}`)
		if rec.Code != c.wantCode || decode[errorResponse](t, rec).Error.Code != c.wantErr {
			t.Errorf("%v: %d, want %d %s", c.err, rec.Code, c.wantCode, c.wantErr)
		}
		if len(rt.endedUsers) != 0 {
			t.Errorf("%v: connections closed although the ban failed", c.err)
		}
	}
}

func TestMemberEndpointsNeedLogin(t *testing.T) {
	h := chatHandler(fakeChat{})
	for _, r := range [][2]string{{"GET", "/api/v1/users"}, {"PATCH", "/api/v1/users/2"}, {"POST", "/api/v1/users/2/kick"}, {"POST", "/api/v1/users/2/ban"}, {"DELETE", "/api/v1/users/2/ban"}} {
		if rec := send(t, h, r[0], r[1], "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d", r[0], r[1], rec.Code)
		}
	}
}

func TestBannedLogin(t *testing.T) {
	acc := fakeAccounts{err: &accounts.BannedError{Reason: "spam"}}
	rec := send(t, handlerWith(acc), "POST", "/api/v1/login", "", `{"username":"x","password":"y"}`)
	body := decode[errorResponse](t, rec)
	if rec.Code != http.StatusForbidden || body.Error.Code != "account_banned" || body.Error.Message != "this account is banned: spam" {
		t.Errorf("status %d, body %+v", rec.Code, body)
	}
}
