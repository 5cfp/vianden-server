// Package files stores uploaded files (M6) on the server's disk.
//
// Security rules, in short:
//   - A file is stored under a RANDOM name; the user's filename is only shown, never used
//     as a path (no "../../etc/passwd" tricks).
//   - What a file IS is decided by the server from its content (the first bytes), never by
//     its name or what the client says. Only real PNG/JPEG/GIF/WebP images count as images.
//   - Everything else is served as a plain download (see the API handler), so a file can
//     never run as a web page or script in a browser.
//   - Size is limited, and so is how many unattached uploads one user can park.
//   - Photo metadata (GPS position, camera, comments) is removed (see metadata.go).
package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif" // register the decoders DecodeConfig can use
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "golang.org/x/image/webp"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/db"
)

const (
	MaxSize        = 25 << 20 // 25 MiB per file
	maxPending     = 20       // unattached uploads per user
	maxFilename    = 200      // characters
	maxImageSide   = 12000    // pixels; bigger "images" are served as plain files
	maxImagePixels = 50_000_000
	uploadsAtOnce  = 4 // uploads are held in memory while checked: at most 4 × 25 MiB
)

var (
	ErrTooLarge       = errors.New("file is larger than 25 MB")
	ErrTooManyPending = errors.New("too many uploads that are not attached to a message yet")
	ErrNotFound       = errors.New("attachment not found")
)

// imageTypes are the only content types shown inline (as pictures) by clients.
var imageTypes = map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/webp": "webp"}

// storageKeyPattern: what a file name on disk must look like (checked again before use).
var storageKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Attachment is an uploaded file as clients see it.
type Attachment struct {
	ID          int64
	Filename    string
	ContentType string // an image type, or application/octet-stream
	Size        int64
	Width       int // images only, else 0
	Height      int
}

// IsImage reports whether clients should show it as a picture.
func (a Attachment) IsImage() bool { return imageTypes[a.ContentType] != "" }

type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	dir     string
	slots   chan struct{} // limits uploads processed at the same time
}

// NewService stores files in <dataDir>/uploads (created if missing, only readable by the
// server's own user).
func NewService(pool *pgxpool.Pool, dataDir string) (*Service, error) {
	dir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Service{pool: pool, queries: db.New(pool), dir: dir, slots: make(chan struct{}, uploadsAtOnce)}, nil
}

// Upload checks and stores a file for `by`. It is not attached to a message yet.
func (s *Service) Upload(ctx context.Context, by accounts.User, filename string, body io.Reader) (Attachment, error) {
	name, err := cleanFilename(filename)
	if err != nil {
		return Attachment{}, err
	}
	pending, err := s.queries.CountPendingUploads(ctx, &by.ID)
	if err != nil {
		return Attachment{}, err
	}
	if pending >= maxPending {
		return Attachment{}, ErrTooManyPending
	}

	select { // wait for a free slot (or give up if the client goes away)
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Attachment{}, ctx.Err()
	}

	// Read at most MaxSize+1 bytes: one byte more tells us the file is too big.
	data, err := io.ReadAll(io.LimitReader(body, MaxSize+1))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return Attachment{}, ErrTooLarge
		}
		return Attachment{}, err
	}
	if len(data) > MaxSize {
		return Attachment{}, ErrTooLarge
	}
	if len(data) == 0 {
		return Attachment{}, &accounts.ValidationError{Field: "file", Message: "the file is empty"}
	}

	a := Attachment{Filename: name, ContentType: "application/octet-stream"}
	if kind, w, h, ok := imageInfo(data); ok {
		cleaned, err := stripMetadata(kind, data)
		if err == nil {
			data = cleaned
			a.ContentType, a.Width, a.Height = "image/"+kind, w, h
		}
		// A file that only LOOKS like an image is kept, but as a plain download.
	}
	a.Size = int64(len(data))

	key, err := randomKey()
	if err != nil {
		return Attachment{}, err
	}
	path := filepath.Join(s.dir, key)
	// O_EXCL: never overwrite an existing file. 0600: only the server can read it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path = upload dir + our own random hex name
	if err != nil {
		return Attachment{}, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return Attachment{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return Attachment{}, err
	}

	row, err := s.queries.CreateAttachment(ctx, db.CreateAttachmentParams{
		UploaderID: &by.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size,
		Width: optionalInt(a.Width), Height: optionalInt(a.Height), StorageKey: key,
	})
	if err != nil {
		os.Remove(path)
		return Attachment{}, err
	}
	a.ID = row.ID
	return a, nil
}

// imageInfo reports whether data is a supported image (by its content), its format and size.
// Images that are absurdly large (a "decompression bomb": a small file that expands to
// gigabytes of pixels) are not treated as images, so clients never try to display them.
func imageInfo(data []byte) (kind string, w, h int, ok bool) {
	kind = imageTypes[http.DetectContentType(data)]
	if kind == "" {
		return "", 0, 0, false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != kind {
		return "", 0, 0, false
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxImageSide || cfg.Height > maxImageSide ||
		cfg.Width*cfg.Height > maxImagePixels {
		return "", 0, 0, false
	}
	return kind, cfg.Width, cfg.Height, true
}

// stripMetadata removes photo metadata where we know how (JPEG, PNG). GIF has no place
// for GPS data; WebP metadata is rare and left as is (documented in docs/API.md).
func stripMetadata(kind string, data []byte) ([]byte, error) {
	switch kind {
	case "jpeg":
		return stripJPEG(data)
	case "png":
		return stripPNG(data)
	}
	return data, nil
}

// cleanFilename keeps only the last part of a path, without control or invisible
// characters, at most 200 characters. It is only ever DISPLAYED.
func cleanFilename(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1 // drop it
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", &accounts.ValidationError{Field: "filename", Message: "a file name is required"}
	}
	if utf8.RuneCountInString(name) > maxFilename {
		// Keep the end (the extension), cut the middle.
		r := []rune(name)
		name = string(r[:maxFilename-20]) + "…" + string(r[len(r)-19:])
	}
	return name, nil
}

func randomKey() (string, error) {
	b := make([]byte, 32) // 256 bits: impossible to guess
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func optionalInt(n int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(n), Valid: n > 0} // #nosec G115 -- n <= maxImageSide
}

// Download is an attachment to send, and what the caller must check first.
type Download struct {
	Attachment
	UploaderID int64 // 0 if that account is gone
	ChannelID  int64 // 0 while not attached to a message: then only the uploader may see it
	key        string
}

// Find looks up an attachment for downloading. Files of deleted messages are gone.
func (s *Service) Find(ctx context.Context, id int64) (Download, error) {
	r, err := s.queries.GetAttachmentForDownload(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Download{}, ErrNotFound
	}
	if err != nil {
		return Download{}, err
	}
	if r.MessageDeleted {
		return Download{}, ErrNotFound
	}
	d := Download{
		Attachment: Attachment{ID: r.ID, Filename: r.Filename, ContentType: r.ContentType, Size: r.Size,
			Width: int(r.Width.Int32), Height: int(r.Height.Int32)},
		key: r.StorageKey,
	}
	if r.UploaderID != nil {
		d.UploaderID = *r.UploaderID
	}
	if r.ChannelID != nil {
		d.ChannelID = *r.ChannelID
	}
	return d, nil
}

// Open opens the file of a Download (the caller closes it).
func (s *Service) Open(d Download) (io.ReadSeekCloser, error) {
	if !storageKeyPattern.MatchString(d.key) { // defense in depth: never a path from elsewhere
		return nil, ErrNotFound
	}
	return os.Open(filepath.Join(s.dir, d.key))
}

// Cleanup deletes uploads never attached to a message (after an hour), and files on disk
// that no attachment points to any more (deleted messages and channels). Returns how many
// files were removed.
func (s *Service) Cleanup(ctx context.Context) (int, error) {
	if _, err := s.queries.DeleteStaleUploads(ctx); err != nil {
		return 0, err
	}
	removedAvatars, err := s.cleanupAvatars(ctx)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	// Only files older than an hour: a brand-new upload may not be in the database yet.
	var candidates []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || time.Since(info.ModTime()) < time.Hour || !storageKeyPattern.MatchString(e.Name()) {
			continue
		}
		candidates = append(candidates, e.Name())
	}
	removed := removedAvatars
	for len(candidates) > 0 {
		batch := candidates[:min(500, len(candidates))]
		candidates = candidates[len(batch):]
		keep, err := s.queries.ExistingStorageKeys(ctx, batch)
		if err != nil {
			return removed, err
		}
		used := make(map[string]bool, len(keep))
		for _, k := range keep {
			used[k] = true
		}
		for _, k := range batch {
			if !used[k] && os.Remove(filepath.Join(s.dir, k)) == nil {
				removed++
			}
		}
	}
	return removed, nil
}
