package media

// CANT-310 — image ingest: what a POST /uploads?kind=image body goes through
// before any byte of it is stored.
//
// THE CLIENT'S Content-Type IS NEVER READ. The type is sniffed from the bytes,
// the bytes are DECODED, and what is stored is a fresh encode of the pixels:
// nothing the sender wrote into the file (EXIF, GPS above all, XMP, comments,
// an appended payload) survives, because nothing but pixels is carried across.
// That is a stronger property than stripping known tags, and it is why the
// stored hash is of the re-encoded bytes (CANT-47 section 1).
//
// SUPPORTED: JPEG, PNG, GIF (first frame only) and WebP (decode only). JPEG
// comes out as JPEG; the rest come out as PNG. HEIC and AVIF cannot be decoded
// in a CGO_ENABLED=0 binary and are refused with ErrUnsupportedType, as is
// everything else, SVG included: an SVG is a document that can carry script.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF decoder; only the first frame is read
	"image/jpeg"
	"image/png"
	"io"
	"net/http"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder
)

var (
	// ErrUnsupportedType is a body that is not an image this binary can decode:
	// HEIC and AVIF, SVG, a document, or bytes that merely claim to be a JPEG.
	ErrUnsupportedType = errors.New("media: not a supported image type")

	// ErrImageTooLarge is an image whose declared dimensions exceed MaxPixels.
	// It is decided from the header, before the pixels are decoded.
	ErrImageTooLarge = errors.New("media: image dimensions are over the limit")
)

const (
	// MaxPixels bounds width*height, checked on image.DecodeConfig. A 16 MiB
	// file can declare a hundred-thousand-pixel-wide canvas that inflates to
	// gigabytes when decoded; 64 megapixels is a 9000x7000 photograph with room
	// to spare, and about 256 MiB of RGBA at the worst.
	MaxPixels = 64 << 20

	// ThumbEdge is the thumbnail's longest side in pixels.
	ThumbEdge = 320

	jpegQuality  = 90
	thumbQuality = 80
)

// Image is an ingested upload: the bytes to store and what is recorded about
// them.
type Image struct {
	Data   []byte // re-encoded, metadata-free
	Thumb  []byte // the thumbnail, a blob of its own
	Width  int    // after orientation
	Height int
	// Placeholder is the blurhash.
	Placeholder string
	// Ext is the extension the stored bytes want: ".jpg" or ".png".
	Ext string
}

// IngestImage reads r (already bounded by the caller's cap) and ingests it.
func IngestImage(r io.Reader) (*Image, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("media: read image: %w", err)
	}

	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	switch http.DetectContentType(head) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		// HEIC/AVIF are ISO-BMFF ("ftyp" at byte 4) and sniff as octet-stream;
		// either way they land here, and the message does not need to tell them
		// apart.
		return nil, ErrUnsupportedType
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedType, err)
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, ErrImageTooLarge
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedType, err)
	}
	if format == "jpeg" {
		src = orient(src, jpegOrientation(raw))
	}

	b := src.Bounds()
	out := &Image{Width: b.Dx(), Height: b.Dy(), Ext: ".png"}
	asJPEG := format == "jpeg"
	if asJPEG {
		out.Ext = ".jpg"
	}
	if out.Data, err = encode(src, asJPEG, jpegQuality); err != nil {
		return nil, err
	}
	thumb := src
	if out.Width > ThumbEdge || out.Height > ThumbEdge {
		thumb = scale(src, ThumbEdge)
	}
	if out.Thumb, err = encode(thumb, asJPEG, thumbQuality); err != nil {
		return nil, err
	}
	out.Placeholder = Blurhash(scale(src, 32), 4, 3)
	return out, nil
}

func encode(img image.Image, asJPEG bool, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if asJPEG {
		// A JPEG has no alpha, and a decoded JPEG never has any, so this is a
		// straight encode. image/jpeg writes JFIF and nothing else: no EXIF.
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("media: encode jpeg: %w", err)
		}
	} else if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("media: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// scale fits img inside edge x edge, keeping its aspect ratio.
func scale(img image.Image, edge int) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w >= h {
		h, w = max(1, h*edge/w), edge
	} else {
		w, h = max(1, w*edge/h), edge
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

// jpegOrientation returns the EXIF Orientation (1-8) of a JPEG, or 1 when there
// is none or the segment is malformed. It reads only; the value is applied to
// the pixels and the segment is never copied to the output.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 || marker == 0xFF {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // start of scan / end: no more headers
			return 1
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return 1
		}
		if marker == 0xE1 {
			if o := exifOrientation(b[i+4 : i+2+n]); o != 0 {
				return o
			}
		}
		i += 2 + n
	}
	return 1
}

func exifOrientation(seg []byte) int {
	if len(seg) < 14 || string(seg[:6]) != "Exif\x00\x00" {
		return 0
	}
	t := seg[6:]
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:]) != 42 {
		return 0
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + 12*k
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 { // Orientation, SHORT
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

// orient applies an EXIF orientation to the pixels so the stored image is
// upright and needs no tag.
func orient(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6: // rotate 90 clockwise
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8: // rotate 90 counterclockwise
				nx, ny = y, w-1-x
			}
			dst.Set(nx, ny, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
