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
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/5cfp/vianden-server/internal/accounts"
	"github.com/5cfp/vianden-server/internal/db"
)

const (
	MaxAvatarSize  = 5 << 20 // 5 MiB upload
	AvatarPixels   = 256     // the stored avatar is AvatarPixels x AvatarPixels
	maxAvatarInput = 4096    // larger pictures are refused (decoding them costs a lot of memory)
)

// Avatars are re-drawn by the server instead of stored as uploaded:
//   - Only the pixels survive: no metadata (GPS, camera), no hidden extra data, no
//     "image that is also a script" tricks. The result is always a plain PNG.
//   - Everyone downloads a small 256x256 file, whatever was uploaded.
//   - The size is checked BEFORE decoding. Decoding needs width x height x 4 bytes of
//     memory, so a tiny file claiming to be 100000x100000 pixels (a "decompression bomb")
//     would otherwise use 40 GB.

var errNotAnImage = &accounts.ValidationError{Field: "avatar", Message: "must be a PNG, JPEG, GIF or WebP image, at most 4096x4096 pixels"}

// makeAvatar turns an uploaded picture into a square 256x256 PNG (center crop).
func makeAvatar(data []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || imageTypes["image/"+format] == "" {
		return nil, errNotAnImage
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxAvatarInput || cfg.Height > maxAvatarInput {
		return nil, errNotAnImage
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errNotAnImage
	}

	// The largest centered square of the picture.
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	square := image.Rect(x0, y0, x0+side, y0+side)

	dst := image.NewRGBA(image.Rect(0, 0, AvatarPixels, AvatarPixels))
	// Catmull-Rom: a high-quality resampling filter (smooth, not blurry or blocky).
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, square, xdraw.Src, nil)

	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Service) avatarDir() string { return filepath.Join(filepath.Dir(s.dir), "avatars") }

// SetAvatar stores a new avatar for the user. The old avatar file is deleted.
func (s *Service) SetAvatar(ctx context.Context, userID int64, body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, MaxAvatarSize+1))
	if err != nil {
		return ErrTooLarge // the API's body limit stopped it
	}
	if len(data) > MaxAvatarSize {
		return ErrTooLarge
	}

	select { // decoding uses memory: same limit as uploads
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	png, err := makeAvatar(data)
	if err != nil {
		return err
	}

	key, err := randomKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.avatarDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.avatarDir(), key)
	if err := os.WriteFile(path, png, 0o600); err != nil {
		return err
	}
	return s.swapAvatar(ctx, userID, &key, path)
}

// RemoveAvatar deletes the user's avatar.
func (s *Service) RemoveAvatar(ctx context.Context, userID int64) error {
	return s.swapAvatar(ctx, userID, nil, "")
}

// swapAvatar points the user at the new avatar (nil = none) and deletes the old file.
// newPath is removed again if the database update fails.
func (s *Service) swapAvatar(ctx context.Context, userID int64, key *string, newPath string) error {
	old, err := s.queries.GetUser(ctx, userID)
	if err != nil {
		if newPath != "" {
			os.Remove(newPath)
		}
		return accounts.ErrUserNotFound
	}
	_, err = s.queries.SetAvatarKey(ctx, db.SetAvatarKeyParams{ID: userID, AvatarKey: key})
	if err != nil {
		if newPath != "" {
			os.Remove(newPath)
		}
		return err
	}
	if old.AvatarKey != nil && storageKeyPattern.MatchString(*old.AvatarKey) {
		os.Remove(filepath.Join(s.avatarDir(), *old.AvatarKey))
	}
	return nil
}

// OpenAvatar opens an avatar file by its key.
func (s *Service) OpenAvatar(key string) (io.ReadSeekCloser, error) {
	if !storageKeyPattern.MatchString(key) { // never a path from the request
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.avatarDir(), key)) // #nosec G304 -- key checked against storageKeyPattern above
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// cleanupAvatars removes avatar files no user points to (older than an hour; covers
// crashes between writing a file and saving it in the database).
func (s *Service) cleanupAvatars(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(s.avatarDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var candidates []string
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && !e.IsDir() && time.Since(info.ModTime()) > time.Hour && storageKeyPattern.MatchString(e.Name()) {
			candidates = append(candidates, e.Name())
		}
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	used, err := s.queries.ExistingAvatarKeys(ctx, candidates)
	if err != nil {
		return 0, err
	}
	keep := make(map[string]bool, len(used))
	for _, k := range used {
		keep[k] = true
	}
	removed := 0
	for _, k := range candidates {
		if !keep[k] && os.Remove(filepath.Join(s.avatarDir(), k)) == nil {
			removed++
		}
	}
	return removed, nil
}
