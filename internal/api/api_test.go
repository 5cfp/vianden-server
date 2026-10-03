package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/5cfp/vianden-server/internal/buildinfo"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeDB stands in for the real database in tests.
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

var healthyDB = fakeDB{}

// do sends a request to h and returns the recorded response.
func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	return v
}

func TestHealth(t *testing.T) {
	rec := do(t, NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: fakeAccounts{}, Logger: discardLogger}), "GET", "/api/v1/health")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decode[healthResponse](t, rec); got != (healthResponse{Status: "ok", Database: "ok"}) {
		t.Errorf("health = %+v", got)
	}
}

func TestHealthDatabaseDown(t *testing.T) {
	down := fakeDB{err: errors.New("connection refused to 10.0.0.5")}
	rec := do(t, NewHandler(Deps{ServerName: "Test", DB: down, Accounts: fakeAccounts{}, Logger: discardLogger}), "GET", "/api/v1/health")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := decode[healthResponse](t, rec); got != (healthResponse{Status: "unavailable", Database: "unreachable"}) {
		t.Errorf("health = %+v (internal error details must not leak)", got)
	}
}

func TestInfo(t *testing.T) {
	rec := do(t, NewHandler(Deps{ServerName: "My Server", DB: healthyDB, Accounts: fakeAccounts{}, Logger: discardLogger}), "GET", "/api/v1/info")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decode[infoResponse](t, rec)
	want := infoResponse{Name: "My Server", Version: buildinfo.Version, ProtocolVersion: buildinfo.ProtocolVersion}
	if got != want {
		t.Errorf("info = %+v, want %+v", got, want)
	}
}

func TestUnknownRouteReturnsJSON404(t *testing.T) {
	rec := do(t, NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: fakeAccounts{}, Logger: discardLogger}), "GET", "/api/v1/does-not-exist")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := decode[errorResponse](t, rec).Error.Code; got != "not_found" {
		t.Errorf("error code = %q, want not_found", got)
	}
}

func TestPanicReturns500WithoutDetails(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret internal detail")
	})
	rec := do(t, recoverPanics(discardLogger, panicking), "GET", "/")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decode[errorResponse](t, rec)
	if body.Error.Code != "internal_error" || body.Error.Message != "internal server error" {
		t.Errorf("unexpected error body: %+v (panic details must not leak)", body)
	}
}
