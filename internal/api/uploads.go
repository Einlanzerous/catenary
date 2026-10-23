package api

// CANT-309 — `POST /uploads?kind=voice`, the one authenticated request that
// takes media bytes in (CANT-47 ruling 1).
//
// THE BODY IS THE BYTES, RAW, AND THE CLIENT'S Content-Type IS NEVER READ. What
// a blob is gets decided from the bytes (ingest, CANT-310) and recorded in
// Postgres; a header the sender chose is not evidence of anything. The answer is
// the single-use handle a later send names as `upload_id`.
//
// THE CAP IS ENFORCED WHILE STREAMING, not after: an honest Content-Length over
// the cap is refused before a byte is read, and a body that lies about its
// length is cut off by MaxBytesReader at cap+1, so no client can make the
// server hold or write more than the cap for one request.
//
// A route that is not wired is a route that does not exist (see Deps.Upload),
// the same safe absence every other seam here has.

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/media"
)

// uploadHandler serves POST /uploads.
func uploadHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authentication before anything about the request is looked at, so an
		// anonymous caller learns nothing about which kinds or caps exist.
		uploader, ok := d.CallerID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		kind := r.URL.Query().Get("kind")
		if kind == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind is required"})
			return
		}
		max, supported := d.UploadCaps[kind]
		if !supported {
			// 415 for a kind we do not take, whether it is nonsense or a real
			// kind whose ingest has not landed: either way, this body is not a
			// thing this endpoint accepts.
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "kind is not supported"})
			return
		}

		if r.ContentLength > max {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "upload is over the size cap"})
			return
		}

		id, err := d.Upload(r.Context(), uploader, kind, r.URL.Query().Get("filename"), http.MaxBytesReader(w, r.Body, max), max)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.Is(err, media.ErrTooLarge) || errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "upload is over the size cap"})
				return
			}
			// Ingest's refusals (CANT-310): bytes that are not an image this
			// binary can decode, and an image whose declared dimensions are a
			// decompression bomb.
			if errors.Is(err, media.ErrUnsupportedType) {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "not a supported image type (JPEG, PNG, GIF and WebP are)"})
				return
			}
			if errors.Is(err, media.ErrImageTooLarge) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "image dimensions are over the limit"})
				return
			}
			// A client that hung up mid-body is not a server fault and not a
			// log line at error.
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				d.Logger.InfoContext(r.Context(), "upload abandoned", "uploader_id", uploader, "kind", kind)
				return
			}
			d.Logger.ErrorContext(r.Context(), "upload failed", "uploader_id", uploader, "kind", kind, "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "upload failed"))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"upload_id": id.String()})
	}
}

// UploadFunc stores body (at most max bytes) as a blob of the given kind and
// records the handle for uploader. The composition root supplies it; this
// package neither knows the media store nor the database.
//
// filename is the client's suggested name from ?filename=, unsanitized; only an
// image row records one.
type UploadFunc func(ctx context.Context, uploader uuid.UUID, kind, filename string, body io.Reader, max int64) (uuid.UUID, error)
