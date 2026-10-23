package api

// CANT-311 — GET /media/{key}?kid=&sig=[&v=thumb]: the other half of the signed
// URL media.Signer builds. The signature is the credential: no account, no
// session, no expiry (CANT-47 section 3), so this route sits outside CallerID on
// purpose and is registered only with the seams below.
//
// REFUSALS, in the order they are decided: 403 for a missing/bad kid or sig
// (before any lookup, so an unsigned caller learns nothing about which keys
// exist); 400 for a v that is not empty or "thumb"; 404 for a validly signed key
// with no blob, and for v=thumb with no thumbnail — NEVER a fallback to the
// full image.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/magos/catenary/internal/media"
)

// MediaInfo is what the uploads table knows about a stored blob.
type MediaInfo struct {
	Kind     string // voice | image, or "" when no uploads row names the key
	ThumbKey string // "" when there is no thumbnail
}

// MediaInfoFunc looks a storage key up in uploads. Found is false when no row
// has it.
type MediaInfoFunc func(ctx context.Context, storageKey string) (MediaInfo, bool, error)

const mediaCache = "private, max-age=31536000, immutable"

func mediaHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		q := r.URL.Query()
		kid, err := strconv.Atoi(q.Get("kid"))
		if err != nil || !d.MediaSigner.Verify(key, kid, q.Get("sig")) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad signature"})
			return
		}
		variant := q.Get("v")
		if variant != "" && variant != "thumb" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "v must be empty or thumb"})
			return
		}

		info, found, err := d.MediaInfo(r.Context(), key)
		if err != nil {
			d.Logger.ErrorContext(r.Context(), "media lookup failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "media lookup failed"))
			return
		}
		blobKey := key
		if variant == "thumb" {
			if !found || info.ThumbKey == "" {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no thumbnail"})
				return
			}
			blobKey = info.ThumbKey
		}

		blob, _, err := d.MediaStore.Open(r.Context(), blobKey)
		if err != nil {
			if errors.Is(err, media.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			d.Logger.ErrorContext(r.Context(), "media open failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "media open failed"))
			return
		}
		defer blob.Close()

		head := make([]byte, 512)
		n, _ := io.ReadFull(blob, head)
		if _, err := blob.Seek(0, io.SeekStart); err != nil {
			d.Logger.ErrorContext(r.Context(), "media seek failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "media seek failed"))
			return
		}
		kind := info.Kind
		if variant == "thumb" {
			kind = "image"
		}
		ctype, inline := media.ContentType(head[:n], kind)

		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", mediaCache)
		if !inline {
			h.Set("Content-Disposition", "attachment")
		}
		http.ServeContent(w, r, "", time.Time{}, blob)
	}
}
