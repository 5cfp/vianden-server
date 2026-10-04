package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/files"
)

// fakeFiles stores one upload in memory.
type fakeFiles struct {
	err      error
	download files.Download
	content  string
	gotName  *string
	gotBody  *string
}

func (f fakeFiles) Upload(_ context.Context, _ accounts.User, filename string, body io.Reader) (files.Attachment, error) {
	b, err := io.ReadAll(body)
	if f.gotName != nil {
		*f.gotName, *f.gotBody = filename, string(b)
	}
	if err != nil {
		return files.Attachment{}, files.ErrTooLarge // MaxBytesReader stopped it
	}
	if f.err != nil {
		return files.Attachment{}, f.err
	}
	return files.Attachment{ID: 5, Filename: filename, ContentType: "image/png", Size: int64(len(b)), Width: 3, Height: 2}, nil
}

func (f fakeFiles) Find(context.Context, int64) (files.Download, error) {
	if f.err != nil {
		return files.Download{}, f.err
	}
	return f.download, nil
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func (f fakeFiles) Open(files.Download) (io.ReadSeekCloser, error) {
	return nopCloser{strings.NewReader(f.content)}, nil
}

func filesHandler(f fakeFiles, c fakeChat) http.Handler {
	acc := fakeAccounts{validToken: "vs_owner", session: accounts.Session{ID: 1, User: osama}}
	return NewHandler(Deps{ServerName: "Test", DB: healthyDB, Accounts: memberToo{acc}, Chat: c, Files: f, Logger: discardLogger})
}

func TestUpload(t *testing.T) {
	var name, body string
	h := filesHandler(fakeFiles{gotName: &name, gotBody: &body}, fakeChat{})
	rec := send(t, h, "POST", "/api/v1/attachments?filename=cat%20pic.png", "Bearer vs_member", "PNGDATA")
	if rec.Code != http.StatusCreated || name != "cat pic.png" || body != "PNGDATA" {
		t.Fatalf("status %d, name %q, body %q", rec.Code, name, body)
	}
	want := `{"attachment":{"id":5,"filename":"cat pic.png","content_type":"image/png","size":7,"width":3,"height":2}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("\n got: %s\nwant: %s", rec.Body, want)
	}
	if rec := send(t, h, "POST", "/api/v1/attachments?filename=x", "", "data"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no login: %d", rec.Code)
	}
}

func TestUploadErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		want string
	}{
		{files.ErrTooLarge, http.StatusRequestEntityTooLarge, "file_too_large"},
		{files.ErrTooManyPending, http.StatusTooManyRequests, "too_many_uploads"},
		{&accounts.ValidationError{Field: "filename", Message: "x"}, http.StatusBadRequest, "invalid_filename"},
	} {
		rec := send(t, filesHandler(fakeFiles{err: c.err}, fakeChat{}), "POST", "/api/v1/attachments?filename=a", "Bearer vs_member", "x")
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), `"code":"`+c.want+`"`) {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}

func TestUploadBodyIsLimited(t *testing.T) {
	big := strings.Repeat("x", files.MaxSize+2)
	rec := send(t, filesHandler(fakeFiles{}, fakeChat{}), "POST", "/api/v1/attachments?filename=a", "Bearer vs_member", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("25 MB + 2 bytes: %d, want 413", rec.Code)
	}
}

func download(t *testing.T, f fakeFiles, c fakeChat, token string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, filesHandler(f, c), "GET", "/api/v1/attachments/9", token, "")
}

func TestDownloadHeaders(t *testing.T) {
	// A file that is not an image: always a download, never shown by a browser.
	page := fakeFiles{content: "<script>alert(1)</script>", download: files.Download{
		Attachment: files.Attachment{ID: 9, Filename: "évil \"page\".html", ContentType: "application/octet-stream", Size: 25},
		ChannelID:  1,
	}}
	rec := download(t, page, fakeChat{}, "Bearer vs_member")
	if rec.Code != http.StatusOK || rec.Body.String() != page.content {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body)
	}
	for k, want := range map[string]string{
		"Content-Type":            "application/octet-stream",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Content-Disposition":     `attachment; filename*=utf-8''%C3%A9vil%20%22page%22.html`,
		"Cache-Control":           "private, max-age=31536000, immutable",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}

	img := fakeFiles{content: "PNG", download: files.Download{
		Attachment: files.Attachment{ID: 9, Filename: "cat.png", ContentType: "image/png", Size: 3, Width: 1, Height: 1},
		ChannelID:  1,
	}}
	rec = download(t, img, fakeChat{}, "Bearer vs_member")
	if rec.Header().Get("Content-Disposition") != `inline; filename=cat.png` || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("image headers: %v", rec.Header())
	}
}

func TestDownloadAccess(t *testing.T) {
	inChannel := fakeFiles{content: "x", download: files.Download{Attachment: files.Attachment{ID: 9, ContentType: "application/octet-stream", Filename: "a"}, ChannelID: 1}}
	// A channel the user cannot see: 404 (the fake chat reports "not found").
	if rec := download(t, inChannel, fakeChat{err: chat404}, "Bearer vs_member"); rec.Code != http.StatusNotFound {
		t.Errorf("hidden channel: %d", rec.Code)
	}
	// Not sent yet: only the uploader.
	pending := fakeFiles{content: "x", download: files.Download{Attachment: files.Attachment{ID: 9, ContentType: "application/octet-stream", Filename: "a"}, UploaderID: osama.ID}}
	if rec := download(t, pending, fakeChat{}, "Bearer vs_member"); rec.Code != http.StatusNotFound {
		t.Errorf("someone else's pending upload: %d", rec.Code)
	}
	if rec := download(t, pending, fakeChat{}, "Bearer vs_owner"); rec.Code != http.StatusOK {
		t.Errorf("own pending upload: %d", rec.Code)
	}
	if rec := download(t, fakeFiles{err: files.ErrNotFound}, fakeChat{}, "Bearer vs_member"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown: %d", rec.Code)
	}
	if rec := download(t, inChannel, fakeChat{}, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no login: %d", rec.Code)
	}
}

// chat404 makes fakeChat.Channel report "not found".
var chat404 = io.EOF
