package files

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeAvatarCropsAndResizes(t *testing.T) {
	// 600x300: a red left half and a blue right half. The center square (150..450)
	// shows both halves.
	src := image.NewRGBA(image.Rect(0, 0, 600, 300))
	for x := range 600 {
		for y := range 300 {
			c := color.RGBA{255, 0, 0, 255}
			if x >= 300 {
				c = color.RGBA{0, 0, 255, 255}
			}
			src.Set(x, y, c)
		}
	}
	var in bytes.Buffer
	png.Encode(&in, src)

	out, err := makeAvatar(in.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != AvatarPixels || b.Dy() != AvatarPixels {
		t.Fatalf("avatar is %v, want %dx%d", b, AvatarPixels, AvatarPixels)
	}
	if r, _, _, _ := img.At(10, 128).RGBA(); r < 0xF000 {
		t.Error("left side is not red")
	}
	if _, _, b, _ := img.At(245, 128).RGBA(); b < 0xF000 {
		t.Error("right side is not blue")
	}
}

func TestMakeAvatarRefusesBadInput(t *testing.T) {
	var huge bytes.Buffer
	png.Encode(&huge, image.NewGray(image.Rect(0, 0, maxAvatarInput+1, 1)))
	for name, data := range map[string][]byte{
		"text":      []byte("hello"),
		"too large": huge.Bytes(),
		"truncated": pngBytes(t, 50, 50)[:40],
	} {
		if _, err := makeAvatar(data); validationField(err) != "avatar" {
			t.Errorf("%s: %v, want avatar validation error", name, err)
		}
	}
}

func TestSetAndRemoveAvatar(t *testing.T) {
	s, pool, u := setup(t)
	avatarKey := func() string {
		var k *string
		pool.QueryRow(ctx, "SELECT avatar_key FROM users WHERE id = $1", u.ID).Scan(&k)
		if k == nil {
			return ""
		}
		return *k
	}

	if err := s.SetAvatar(ctx, u.ID, bytes.NewReader(pngBytes(t, 40, 40))); err != nil {
		t.Fatal(err)
	}
	first := avatarKey()
	f, err := s.OpenAvatar(first)
	if err != nil {
		t.Fatalf("open avatar: %v", err)
	}
	f.Close()

	// A new avatar replaces the old one; the old file is gone.
	s.SetAvatar(ctx, u.ID, bytes.NewReader(pngBytes(t, 40, 40)))
	if second := avatarKey(); second == first || second == "" {
		t.Errorf("avatar key %q after replacing %q", second, first)
	}
	if _, err := os.Stat(filepath.Join(s.avatarDir(), first)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old avatar file still there: %v", err)
	}

	if err := s.RemoveAvatar(ctx, u.ID); err != nil || avatarKey() != "" {
		t.Errorf("remove: %v, key %q", err, avatarKey())
	}
	if entries, _ := os.ReadDir(s.avatarDir()); len(entries) != 0 {
		t.Errorf("files left: %d", len(entries))
	}

	if err := s.SetAvatar(ctx, u.ID, strings.NewReader(strings.Repeat("x", MaxAvatarSize+1))); !errors.Is(err, ErrTooLarge) {
		t.Errorf("6 MB avatar: %v", err)
	}
	if _, err := s.OpenAvatar("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("path in avatar key: %v", err)
	}
}
