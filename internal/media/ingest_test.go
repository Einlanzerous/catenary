package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
	"testing"
)

// exifSegment builds an APP1 EXIF segment (big-endian) holding Orientation, an
// ImageDescription carrying a sentinel, and a GPSInfo IFD with a latitude, so a
// test can prove none of it comes out the other side.
func exifSegment(orientation uint16, sentinel string) []byte {
	// Layout of the TIFF block: header(8) | IFD0 | GPS IFD | strings.
	be := binary.BigEndian
	desc := append([]byte(sentinel), 0)
	const ifd0Entries = 3
	ifd0Off := 8
	gpsOff := ifd0Off + 2 + 12*ifd0Entries + 4
	gpsEntries := 2
	strOff := gpsOff + 2 + 12*gpsEntries + 4
	tiff := make([]byte, strOff+len(desc)+24)
	copy(tiff, "MM")
	be.PutUint16(tiff[2:], 42)
	be.PutUint32(tiff[4:], uint32(ifd0Off))

	put := func(off int, tag, typ uint16, count, val uint32) {
		be.PutUint16(tiff[off:], tag)
		be.PutUint16(tiff[off+2:], typ)
		be.PutUint32(tiff[off+4:], count)
		be.PutUint32(tiff[off+8:], val)
	}
	be.PutUint16(tiff[ifd0Off:], ifd0Entries)
	e := ifd0Off + 2
	put(e, 0x010E, 2, uint32(len(desc)), uint32(strOff)) // ImageDescription
	put(e+12, 0x0112, 3, 1, uint32(orientation)<<16)     // Orientation (SHORT left-justified)
	put(e+24, 0x8825, 4, 1, uint32(gpsOff))              // GPSInfo pointer
	copy(tiff[strOff:], desc)

	be.PutUint16(tiff[gpsOff:], uint16(gpsEntries))
	g := gpsOff + 2
	put(g, 0x0001, 2, 2, uint32('N')<<24)             // GPSLatitudeRef
	put(g+12, 0x0002, 5, 3, uint32(strOff+len(desc))) // GPSLatitude (rationals)
	copy(tiff[strOff+len(desc):], []byte{0, 0, 0, 47, 0, 0, 0, 1, 0, 0, 0, 36, 0, 0, 0, 1})

	body := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(body)+2))
	return append(seg, body...)
}

// withEXIF splices an APP1 segment in after the SOI marker of a JPEG.
func withEXIF(jpg, seg []byte) []byte {
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

// halves is w x h, left half red and right half blue.
func halves(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, image.Rect(0, 0, w/2, h), image.NewUniform(color.NRGBA{255, 0, 0, 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(w/2, 0, w, h), image.NewUniform(color.NRGBA{0, 0, 255, 255}), image.Point{}, draw.Src)
	return img
}

func jpegOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func isRed(c color.Color) bool  { r, g, b, _ := c.RGBA(); return r>>8 > 200 && g>>8 < 60 && b>>8 < 60 }
func isBlue(c color.Color) bool { r, g, b, _ := c.RGBA(); return b>>8 > 200 && r>>8 < 60 && g>>8 < 60 }

func TestAJPEGWithGPSEXIFComesOutWithNoEXIFAtAll(t *testing.T) {
	const sentinel = "SECRET-LOCATION-47.6N"
	in := withEXIF(jpegOf(t, halves(40, 20)), exifSegment(1, sentinel))

	// The fixture really does carry it, and the parser really does read it.
	if !bytes.Contains(in, []byte(sentinel)) || !bytes.Contains(in, []byte("Exif\x00\x00")) {
		t.Fatal("the fixture does not carry the EXIF it is meant to")
	}

	got, err := IngestImage(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string][]byte{"image": got.Data, "thumbnail": got.Thumb} {
		if bytes.Contains(blob, []byte(sentinel)) {
			t.Errorf("%s still carries the ImageDescription", name)
		}
		if bytes.Contains(blob, []byte("Exif")) {
			t.Errorf("%s still has an Exif header", name)
		}
		// Re-parse: walk the JPEG's segments and list the APPn markers.
		if apps := appSegments(blob); len(apps) != 0 && !(len(apps) == 1 && apps[0] == 0xE0) {
			t.Errorf("%s has application segments %x; want none but JFIF", name, apps)
		}
		if o := jpegOrientation(blob); o != 1 {
			t.Errorf("%s reports orientation %d", name, o)
		}
	}
}

// appSegments lists the APPn marker bytes in a JPEG's header segments.
func appSegments(b []byte) []byte {
	var out []byte
	for i := 2; i+4 <= len(b) && b[i] == 0xFF; {
		m := b[i+1]
		if m == 0xDA {
			break
		}
		if m >= 0xE0 && m <= 0xEF {
			out = append(out, m)
		}
		i += 2 + int(binary.BigEndian.Uint16(b[i+2:]))
	}
	return out
}

func TestOrientationSixIsStoredUprightWithDimensionsSwapped(t *testing.T) {
	// 40 wide x 20 high, red left and blue right, tagged "rotate 90 clockwise to
	// display": upright it is 20 x 40, red on top and blue underneath.
	in := withEXIF(jpegOf(t, halves(40, 20)), exifSegment(6, "x"))
	got, err := IngestImage(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 20 || got.Height != 40 {
		t.Fatalf("recorded %dx%d; want 20x40", got.Width, got.Height)
	}
	img, err := jpeg.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 20 || b.Dy() != 40 {
		t.Fatalf("stored pixels are %dx%d; want 20x40", b.Dx(), b.Dy())
	}
	if !isRed(img.At(10, 5)) || !isBlue(img.At(10, 35)) {
		t.Errorf("not upright: top %v bottom %v; want red over blue", img.At(10, 5), img.At(10, 35))
	}
}

func TestAllEightOrientationsKeepThePixelCount(t *testing.T) {
	for o := uint16(1); o <= 8; o++ {
		in := withEXIF(jpegOf(t, halves(40, 20)), exifSegment(o, "x"))
		got, err := IngestImage(bytes.NewReader(in))
		if err != nil {
			t.Fatalf("orientation %d: %v", o, err)
		}
		swapped := o >= 5
		if swapped && (got.Width != 20 || got.Height != 40) || !swapped && (got.Width != 40 || got.Height != 20) {
			t.Errorf("orientation %d: %dx%d", o, got.Width, got.Height)
		}
	}
}

func TestDimensionsThumbnailAndBlurhashAreRecorded(t *testing.T) {
	got, err := IngestImage(bytes.NewReader(pngOf(t, halves(800, 400))))
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 800 || got.Height != 400 || got.Ext != ".png" {
		t.Errorf("recorded %dx%d %s", got.Width, got.Height, got.Ext)
	}
	th, err := png.Decode(bytes.NewReader(got.Thumb))
	if err != nil {
		t.Fatal(err)
	}
	if b := th.Bounds(); b.Dx() != ThumbEdge || b.Dy() != ThumbEdge/2 {
		t.Errorf("thumbnail is %dx%d; want %dx%d", b.Dx(), b.Dy(), ThumbEdge, ThumbEdge/2)
	}
	if bytes.Equal(got.Thumb, got.Data) || len(got.Thumb) >= len(got.Data) {
		t.Error("the thumbnail is not a smaller blob of its own")
	}
	// 4x3 components: 1 size + 1 max + 4 DC + 2*11 AC = 28 characters.
	if len(got.Placeholder) != 28 {
		t.Errorf("blurhash %q has length %d; want 28", got.Placeholder, len(got.Placeholder))
	}
}

func TestASmallImageIsNotUpscaledForItsThumbnail(t *testing.T) {
	got, err := IngestImage(bytes.NewReader(pngOf(t, halves(40, 20))))
	if err != nil {
		t.Fatal(err)
	}
	th, _ := png.Decode(bytes.NewReader(got.Thumb))
	if b := th.Bounds(); b.Dx() != 40 || b.Dy() != 20 {
		t.Errorf("thumbnail %dx%d; want the original 40x20", b.Dx(), b.Dy())
	}
}

func TestBlurhashDCIsTheAverageColor(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.NRGBA{200, 100, 50, 255}), image.Point{}, draw.Src)
	h := Blurhash(img, 4, 3)
	dc := 0
	for _, c := range h[2:6] {
		dc = dc*83 + strings.IndexRune(b83chars, c)
	}
	r, g, b := dc>>16, (dc>>8)&255, dc&255
	for name, d := range map[string]int{"r": r - 200, "g": g - 100, "b": b - 50} {
		if math.Abs(float64(d)) > 1 {
			t.Errorf("DC %s off by %d (got %d,%d,%d)", name, d, r, g, b)
		}
	}
}

func TestBodiesThatAreNotSupportedImagesAreRefused(t *testing.T) {
	jpegMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}
	cases := map[string][]byte{
		"text that a client labelled image/jpeg": []byte("this is not an image, whatever the header said"),
		"an SVG":                                 []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"an SVG with an XML prolog":              []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`),
		"HEIC":                                   append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...),
		"AVIF":                                   append([]byte{0, 0, 0, 24}, []byte("ftypavif\x00\x00\x00\x00mif1avif")...),
		"a truncated JPEG header":                jpegMagic,
		"a PDF":                                  []byte("%PDF-1.7\n"),
		"nothing":                                {},
	}
	for name, body := range cases {
		_, err := IngestImage(bytes.NewReader(body))
		if !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s: err = %v; want ErrUnsupportedType", name, err)
		}
	}
}

// A PNG header declaring 100000 x 100000 is a few hundred bytes on disk and
// 40 GB decoded. It must be refused from the header.
func TestADecompressionBombIsRefusedBeforeItIsDecoded(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 100000)
	binary.BigEndian.PutUint32(ihdr[4:], 100000)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		binary.Write(&b, binary.BigEndian, crc.Sum32())
	}
	chunk("IHDR", ihdr)
	chunk("IEND", nil)

	_, err := IngestImage(bytes.NewReader(b.Bytes()))
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v; want ErrImageTooLarge", err)
	}
}

func TestAnAnimatedGIFIngestsAsItsFirstFrame(t *testing.T) {
	// A one-frame GIF is enough: the decoder registration is what is under test.
	var b bytes.Buffer
	if err := encodeTinyGIF(&b); err != nil {
		t.Fatal(err)
	}
	got, err := IngestImage(&b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ext != ".png" || got.Width != 4 || got.Height != 4 {
		t.Errorf("got %s %dx%d", got.Ext, got.Width, got.Height)
	}
}
