package files

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Photos often carry hidden metadata: GPS coordinates of where they were taken, the camera
// or phone model, the owner's name, editing software, even a small preview of the original
// before it was cropped. Sharing a photo in a chat should not share that. These functions
// remove it WITHOUT re-encoding the image (no quality loss): JPEG and PNG files are made of
// separate blocks, and we copy every block except the metadata ones.

var errBadImage = errors.New("malformed image")

// stripJPEG removes metadata segments from a JPEG: APP1 (EXIF, XMP), APP12/APP13 (Photoshop,
// IPTC: names, captions, locations), and COM (comments). It keeps APP0 (JFIF), APP2 (the
// color profile, needed for correct colors) and APP14 (Adobe color info).
//
// One EXIF value matters for display: Orientation (phones store photos sideways and say
// "rotate me" in EXIF). If the photo had one, a tiny new EXIF block with ONLY that value
// is written back, so the picture is not shown rotated.
func stripJPEG(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errBadImage
	}
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write(data[:2]) // SOI (start of image)
	orientation := 0

	i := 2
	for {
		if i+4 > len(data) || data[i] != 0xFF {
			return nil, errBadImage
		}
		marker := data[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA { // SOS: the image data follows; copy it up to the end marker
			if orientation > 1 {
				writeOrientationSegment(out, orientation)
			}
			// EOI (FF D9) cannot appear inside the image data (a 0xFF byte there is always
			// followed by 00 or a restart marker), so the first one is the real end.
			// Anything after it is not part of the picture and is dropped: files that are
			// "a JPEG and something else" at once (polyglots) hide data there.
			end := bytes.Index(data[i:], []byte{0xFF, 0xD9})
			if end < 0 {
				return nil, errBadImage
			}
			out.Write(data[i : i+end+2])
			return out.Bytes(), nil
		}
		length := int(binary.BigEndian.Uint16(data[i+2 : i+4])) // includes the 2 length bytes
		if length < 2 || i+2+length > len(data) {
			return nil, errBadImage
		}
		segment := data[i : i+2+length]
		payload := data[i+4 : i+2+length]

		switch {
		case marker == 0xE1: // APP1: EXIF or XMP. Dropped, but remember the orientation.
			if o := exifOrientation(payload); o > 0 {
				orientation = o
			}
		case marker == 0xEC || marker == 0xED || marker == 0xFE: // APP12, APP13, COM
		default:
			out.Write(segment)
		}
		i += 2 + length
	}
}

// exifOrientation reads the Orientation tag (1-8) from an APP1 payload; 0 if absent.
// EXIF is a small TIFF file: a byte-order mark, then a table of tags (IFD0).
func exifOrientation(p []byte) int {
	if len(p) < 14 || !bytes.Equal(p[:6], []byte("Exif\x00\x00")) {
		return 0
	}
	tiff := p[6:]
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 0
	}
	count := int(order.Uint16(tiff[ifd : ifd+2]))
	for n := range count {
		e := ifd + 2 + n*12 // each entry: tag(2) type(2) count(4) value(4)
		if e+12 > len(tiff) {
			return 0
		}
		if order.Uint16(tiff[e:e+2]) == 0x0112 { // Orientation, a SHORT
			if o := int(order.Uint16(tiff[e+8 : e+10])); o >= 1 && o <= 8 {
				return o
			}
			return 0
		}
	}
	return 0
}

// writeOrientationSegment writes an APP1 EXIF segment that contains only Orientation.
func writeOrientationSegment(out *bytes.Buffer, orientation int) {
	tiff := []byte{
		'M', 'M', 0x00, 0x2A, // big-endian TIFF
		0x00, 0x00, 0x00, 0x08, // IFD0 starts at byte 8
		0x00, 0x01, // 1 entry
		0x01, 0x12, 0x00, 0x03, // tag 0x0112 (Orientation), type 3 (SHORT)
		0x00, 0x00, 0x00, 0x01, // count 1
		0x00, byte(orientation), 0x00, 0x00, // #nosec G115 -- the value; orientation is 2-8 (checked in exifOrientation)
		0x00, 0x00, 0x00, 0x00, // no next IFD
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(out, binary.BigEndian, uint16(len(payload)+2)) // #nosec G115 -- always 34 bytes
	out.Write(payload)
}

// pngMetadataChunks are text and EXIF chunks: they can hold anything (authors, locations,
// software, comments). Image data, colors and transparency are in other chunks.
var pngMetadataChunks = map[string]bool{"eXIf": true, "tEXt": true, "iTXt": true, "zTXt": true, "tIME": true}

// stripPNG removes metadata chunks from a PNG. Each chunk carries its own checksum, so
// copying the other chunks unchanged keeps the file valid.
func stripPNG(data []byte) ([]byte, error) {
	signature := []byte("\x89PNG\r\n\x1a\n")
	if !bytes.HasPrefix(data, signature) {
		return nil, errBadImage
	}
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write(signature)
	i := len(signature)
	for i < len(data) {
		if i+12 > len(data) { // length(4) type(4) ... crc(4)
			return nil, errBadImage
		}
		length := int(binary.BigEndian.Uint32(data[i : i+4]))
		end := i + 12 + length
		if length < 0 || end > len(data) || end < i {
			return nil, errBadImage
		}
		kind := string(data[i+4 : i+8])
		if !pngMetadataChunks[kind] {
			out.Write(data[i:end])
		}
		i = end
		if kind == "IEND" {
			break
		}
	}
	return out.Bytes(), nil
}
