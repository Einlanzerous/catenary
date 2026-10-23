package main

// CANT-310 — the image half of POST /uploads, kept out of setup() so the
// composition root only says WHICH kinds exist.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/media"
	"github.com/magos/catenary/internal/store"
)

// ingestImageUpload runs the body through media.IngestImage and stores the
// result: the re-encoded image and its thumbnail as two blobs, one uploads row
// that is READY (dimensions, blurhash and filename all set).
//
// The cap was already enforced on the body by the handler's MaxBytesReader; the
// two Put calls are capped at their own length, which is what they are.
//
// A failure after the first Put leaves a blob no row names. Garbage collection
// (CANT-307) is what collects it, which is why nothing here deletes.
func ingestImageUpload(ctx context.Context, ms media.Store, st *store.Store, uploader uuid.UUID, filename string, body io.Reader) (uuid.UUID, error) {
	img, err := media.IngestImage(body)
	if err != nil {
		return uuid.Nil, err
	}
	key, n, err := ms.Put(ctx, bytes.NewReader(img.Data), int64(len(img.Data)))
	if err != nil {
		return uuid.Nil, err
	}
	thumbKey, _, err := ms.Put(ctx, bytes.NewReader(img.Thumb), int64(len(img.Thumb)))
	if err != nil {
		return uuid.Nil, err
	}
	name := safeFilename(filename, img.Ext)
	w, h := img.Width, img.Height
	return st.CreateUpload(ctx, store.NewUpload{
		Uploader: uploader, Kind: "image", StorageKey: key, Bytes: n,
		ThumbKey: &thumbKey, Width: &w, Height: &h, Placeholder: &img.Placeholder, Filename: &name,
	})
}

// maxFilenameRunes bounds a recorded filename.
const maxFilenameRunes = 120

// safeFilename turns the client's suggestion into something safe to record and
// later show: the last path element only, no control or formatting characters,
// bounded. The name is a label and never a path (blobs are named by hash), so
// this is about what a client might render, not about the disk. An empty result
// falls back to "image" plus the extension of the bytes actually stored.
func safeFilename(raw, ext string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "")
	}
	raw = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, raw)
	raw = strings.TrimSpace(raw)
	if r := []rune(raw); len(r) > maxFilenameRunes {
		raw = string(r[:maxFilenameRunes])
	}
	if raw == "" || raw == "." || raw == ".." {
		return "image" + ext
	}
	return raw
}
