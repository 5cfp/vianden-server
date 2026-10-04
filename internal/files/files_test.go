package files

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/perm"
	"github.com/5cfp/vianden-server/internal/testdb"
)

var ctx = context.Background()

func setup(t *testing.T) (*Service, *pgxpool.Pool, accounts.User) {
	t.Helper()
	pool := testdb.New(t)
	u := accounts.User{Username: "friend", DisplayName: "Friend", Role: perm.Member}
	if err := pool.QueryRow(ctx, "INSERT INTO users (username, display_name, password_hash, role) VALUES ('friend', 'Friend', 'x', 'member') RETURNING id").Scan(&u.ID); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(pool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, pool, u
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func validationField(err error) string {
	var ve *accounts.ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return ""
}

func TestUploadImage(t *testing.T) {
	s, _, u := setup(t)
	a, err := s.Upload(ctx, u, `C:\Users\me\Pictures\cat.png`, bytes.NewReader(pngBytes(t, 30, 20)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Filename != "cat.png" || a.ContentType != "image/png" || a.Width != 30 || a.Height != 20 || !a.IsImage() {
		t.Errorf("attachment %+v", a)
	}

	d, err := s.Find(ctx, a.ID)
	if err != nil || d.UploaderID != u.ID || d.ChannelID != 0 {
		t.Fatalf("find: %+v, %v", d, err)
	}
	f, err := s.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, _ := io.ReadAll(f); int64(len(got)) != a.Size {
		t.Errorf("stored %d bytes, size says %d", len(got), a.Size)
	}
	// The name on disk is random, not the user's file name.
	entries, _ := os.ReadDir(s.dir)
	if len(entries) != 1 || strings.Contains(entries[0].Name(), "cat") || !storageKeyPattern.MatchString(entries[0].Name()) {
		t.Errorf("files on disk: %v", entries)
	}
}

func TestTheContentDecidesTheType(t *testing.T) {
	s, _, u := setup(t)
	// A web page pretending to be a picture: stored, but only ever as a download.
	a, err := s.Upload(ctx, u, "cute.png", strings.NewReader("<html><script>alert(1)</script></html>"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentType != "application/octet-stream" || a.IsImage() {
		t.Errorf("fake image got type %q", a.ContentType)
	}
	// A real PNG with a wrong name is still an image.
	if a, _ := s.Upload(ctx, u, "notes.txt", bytes.NewReader(pngBytes(t, 2, 2))); a.ContentType != "image/png" {
		t.Errorf("real png named .txt: %q", a.ContentType)
	}
	// A truncated "PNG" (right first bytes, broken inside) is not shown as an image.
	if a, _ := s.Upload(ctx, u, "broken.png", bytes.NewReader(pngBytes(t, 2, 2)[:30])); a.IsImage() {
		t.Error("a broken png was accepted as an image")
	}
}

func TestHugeImagesAreNotImages(t *testing.T) {
	s, _, u := setup(t)
	// 20000 x 1 pixels: too wide. (A real "bomb" is a tiny file claiming billions of pixels.)
	a, err := s.Upload(ctx, u, "wide.png", bytes.NewReader(pngBytes(t, 20000, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if a.IsImage() {
		t.Error("an oversized image is shown inline")
	}
}

func TestUploadLimits(t *testing.T) {
	s, _, u := setup(t)
	if _, err := s.Upload(ctx, u, "big.bin", io.LimitReader(zeros{}, MaxSize+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("25 MB + 1 byte: %v, want ErrTooLarge", err)
	}
	if _, err := s.Upload(ctx, u, "empty.txt", strings.NewReader("")); validationField(err) != "file" {
		t.Errorf("empty file: %v", err)
	}
	for _, name := range []string{"", "  ", "../", "..", "\u202e\u0000"} {
		if _, err := s.Upload(ctx, u, name, strings.NewReader("x")); validationField(err) != "filename" {
			t.Errorf("name %q: %v, want filename error", name, err)
		}
	}
	for range maxPending {
		if _, err := s.Upload(ctx, u, "a.txt", strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Upload(ctx, u, "one-too-many.txt", strings.NewReader("x")); !errors.Is(err, ErrTooManyPending) {
		t.Errorf("upload %d: %v, want ErrTooManyPending", maxPending+1, err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestCleanFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                      "report.pdf",
		"../../etc/passwd":                "passwd",
		`..\..\windows\win.ini`:           "win.ini",
		"evil\u202egpj.exe":               "evilgpj.exe", // right-to-left override removed
		"  spaced name.txt  ":             "spaced name.txt",
		"تقرير.pdf":                       "تقرير.pdf",
		strings.Repeat("a", 250) + ".zip": strings.Repeat("a", 180) + "…" + strings.Repeat("a", 15) + ".zip",
	} {
		if got, err := cleanFilename(in); err != nil || got != want {
			t.Errorf("cleanFilename(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCleanupRemovesUnusedFiles(t *testing.T) {
	s, pool, u := setup(t)
	a, _ := s.Upload(ctx, u, "a.txt", strings.NewReader("stale upload"))
	keep, _ := s.Upload(ctx, u, "b.txt", strings.NewReader("fresh upload"))
	// Make the first upload and both files "old".
	pool.Exec(ctx, "UPDATE attachments SET created_at = now() - interval '2 hours' WHERE id = $1", a.ID)
	old := time.Now().Add(-2 * time.Hour)
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		os.Chtimes(filepath.Join(s.dir, e.Name()), old, old)
	}

	n, err := s.Cleanup(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup removed %d, %v; want 1 (the stale upload)", n, err)
	}
	if _, err := s.Find(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale upload still found: %v", err)
	}
	d, err := s.Find(ctx, keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := s.Open(d); err != nil {
		t.Errorf("fresh upload's file removed: %v", err)
	} else {
		f.Close()
	}
}
