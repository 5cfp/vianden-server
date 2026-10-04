package files

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// testJPEG makes a real 8x4 JPEG, optionally with an EXIF block (containing a fake GPS
// string and an orientation) and a comment inserted after the start marker.
func testJPEG(t *testing.T, orientation int, extra ...[]byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	var out bytes.Buffer
	out.Write(raw[:2])
	if orientation > 0 {
		var app1 bytes.Buffer
		writeOrientationSegment(&app1, orientation)
		seg := app1.Bytes()
		// Put a fake GPS string inside the EXIF payload to prove it disappears.
		payload := append(append([]byte{}, seg[4:]...), []byte("GPS 52.37N 4.89E")...)
		out.Write([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)})
		out.Write(payload)
	}
	for _, e := range extra {
		out.Write(e)
	}
	out.Write(raw[2:])
	return out.Bytes()
}

func TestStripJPEG(t *testing.T) {
	comment := append([]byte{0xFF, 0xFE, 0x00, 0x0B}, []byte("secret!!!")...)
	in := testJPEG(t, 6, comment)
	in = append(in, []byte("PK\x03\x04 hidden zip after the end")...)

	out, err := stripJPEG(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"GPS", "secret", "hidden zip"} {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("%q is still in the file", s)
		}
	}
	if o := exifOrientation(findAPP1(t, out)); o != 6 {
		t.Errorf("orientation %d, want 6 kept", o)
	}
	// Still a valid JPEG of the same size.
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 8 || cfg.Height != 4 {
		t.Errorf("decode after stripping: %+v, %v", cfg, err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("full decode: %v", err)
	}
}

func TestStripJPEGWithoutOrientationAddsNothing(t *testing.T) {
	out, err := stripJPEG(testJPEG(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("Exif")) {
		t.Error("an EXIF block was added to a photo that had none")
	}
}

func TestStripJPEGRejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("not a jpeg"), {0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, {0xFF, 0xD8}} {
		if _, err := stripJPEG(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func findAPP1(t *testing.T, data []byte) []byte {
	t.Helper()
	i := bytes.Index(data, []byte{0xFF, 0xE1})
	if i < 0 {
		t.Fatal("no APP1 segment")
	}
	n := int(data[i+2])<<8 | int(data[i+3])
	return data[i+4 : i+2+n]
}

func TestStripPNG(t *testing.T) {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2)))
	raw := buf.Bytes()
	// Insert a tEXt chunk (with a made-up CRC: we never check it, we drop the chunk) after IHDR.
	ihdrEnd := 8 + 12 + 13
	text := []byte("Author\x00Osama, at home")
	chunk := append([]byte{0, 0, 0, byte(len(text))}, append([]byte("tEXt"), append(text, 0, 0, 0, 0)...)...)
	in := append(append(append([]byte{}, raw[:ihdrEnd]...), chunk...), raw[ihdrEnd:]...)
	in = append(in, []byte("trailing junk")...)

	out, err := stripPNG(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("Osama")) || bytes.Contains(out, []byte("junk")) {
		t.Error("metadata or trailing data kept")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("decode after stripping: %v", err)
	}
}

func TestStripPNGRejectsGarbage(t *testing.T) {
	bad := append([]byte("\x89PNG\r\n\x1a\n"), 0xFF, 0xFF, 0xFF, 0xFF, 'I', 'D', 'A', 'T')
	if _, err := stripPNG(bad); err == nil {
		t.Error("truncated chunk accepted")
	}
}
