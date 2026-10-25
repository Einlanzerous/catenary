package api

// CANT-322 (CANT-308 row 3) — `POST /link-previews`, the one authenticated
// request that makes the server open a socket to an address a member chose.
//
// WHAT THIS FILE OWNS is the shape of the answer, and nothing about fetching:
//
//   - 401 is decided FIRST, before the body is read, so an anonymous caller
//     learns nothing, not even whether the route does anything.
//   - 400 is about the syntax the client sent (malformed JSON, a URL that is not
//     http or https, a port other than 80 or 443, userinfo, over 2048 bytes). It
//     reveals nothing about the network, so it can differ from the rest.
//   - 429 is the abuse bound, with retry_after_sec in the wire ServerError.
//   - 200 is a card. EVERY OTHER OUTCOME OF THE FETCH IS THE SAME EMPTY 204: a
//     blocked address, a timeout, a page that is not HTML, a host that does not
//     resolve and a page with nothing on it all look identical on the wire, so
//     the endpoint cannot be used to ask which internal addresses answer. The
//     class goes to the server log and nowhere else.
//
// A route that is not wired does not exist (the Deps.Upload precedent): with
// CATENARY_LINK_PREVIEWS unset the router answers its own 404 and a client
// treats that as "previews unavailable".

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// LinkPreviewResult is what the composition root's service answers. At most one
// of the three is meaningful: BadURL, a positive RetryAfterSec, or a non-nil
// Card; none of them is the uniform 204.
type LinkPreviewResult struct {
	BadURL        bool
	RetryAfterSec int64
	Card          *wire.LinkPreviewResponse
}

// LinkPreviewFunc resolves rawURL for requester. An error is OUR fault (the
// database, the media store) and is a 500; a fetch failure is never an error.
type LinkPreviewFunc func(ctx context.Context, requester uuid.UUID, rawURL string) (LinkPreviewResult, error)

// maxLinkPreviewBody bounds the request: a URL is at most 2048 bytes, and JSON
// escaping can multiply that, so 16 KiB is generous and still small.
const maxLinkPreviewBody = 16 << 10

func linkPreviewHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requester, ok := d.CallerID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLinkPreviewBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request body could not be read"})
			return
		}
		var req wire.LinkPreviewRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body is not a link preview request"})
			return
		}

		res, err := d.LinkPreview(r.Context(), requester, req.URL)
		switch {
		case err != nil:
			// A client that hung up is not a server fault.
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				return
			}
			d.Logger.ErrorContext(r.Context(), "link preview failed", "requester_id", requester, "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "link preview failed"))
		case res.BadURL:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the url is malformed or not an http or https address"})
		case res.RetryAfterSec > 0:
			// Built from the store's table row like every other refusal, so the
			// code, retryable and status are not decided a second time here.
			var se *store.SendError
			_ = errors.As(store.SendErrorFor(store.ErrRateLimited), &se)
			secs := res.RetryAfterSec
			limited := *se
			limited.RetryAfterSec = &secs
			w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
			writeJSON(w, limited.HTTPStatus(), limited.Wire("too many link previews; try again shortly"))
		case res.Card == nil:
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusOK, res.Card)
		}
	}
}
