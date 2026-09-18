package api

// CANT-117 — the self-service device surface: `GET /devices` and
// `POST /devices/{id}/revoke`.
//
// SELF-SERVICE, NOT ADMIN. Both routes act on the CALLER'S OWN account, and the
// account comes from the credential rather than from anything in the request —
// there is no user id in either path, query or body, so there is nothing for a
// caller to substitute. The admin surface is CANT-69's and lives behind
// Cloudflare Access, which CANT-32 keeps away from these endpoints entirely.
//
// THE OWNERSHIP CHECK IS IN THE STORE. store.RevokeOwnDevice carries the
// user_id in its WHERE clause; this file passes the caller's id and never
// decides anything. store.RevokeDevice — the unscoped administrative revoke
// R6's Deprovision needs — is deliberately not reachable from here.

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wireview"
)

// devicesHandler serves GET /devices: the caller's own devices, oldest first,
// revoked ones included.
//
// A BOT GETS AN EMPTY LIST RATHER THAN A REFUSAL. A bot has no device and never
// enrols one, so its account simply has nothing to list — which is a different
// thing from a caller who may not look, and the wire's own DeviceListResponse
// description says so. Nothing here special-cases it: the query is scoped by
// user id and a bot's returns no rows, which is the honest reason for the empty
// page rather than a branch that fakes one.
func devicesHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		viewer, ok := d.CallerID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		rows, err := d.Devices(r.Context(), viewer)
		if err != nil {
			d.Logger.ErrorContext(r.Context(), "device list failed", "viewer_id", viewer, "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "device list failed"))
			return
		}
		writeJSON(w, http.StatusOK, wireview.DeviceList(rows))
	}
}

// revokeDeviceHandler serves POST /devices/{id}/revoke.
//
// ONE ANSWER FOR NOT-YOURS, UNKNOWN AND ALREADY-REVOKED. The store returns
// false for all three and this returns 204 for all three, which is deliberate:
// telling them apart would make this route an oracle for which device ids exist
// on other accounts, and CANT-28 gives the same reasoning for the credential
// routes having one refusal shape. It also keeps the route idempotent, which is
// what a person double-tapping a button on a phone needs.
//
// 204 RATHER THAN THE DEVICE. There is nothing useful to return — the caller
// knows which device they revoked, and serving the row back would invite a
// client to render a revoked device from the response instead of re-reading the
// list that actually changed.
//
// REVOKING THE DEVICE YOU ARE CALLING FROM IS ALLOWED and is not special-cased.
// It is the "I am signed in on the phone I am about to lose" case, and the
// severance CANT-30 built will close this caller's own socket a moment later,
// which is correct.
func revokeDeviceHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		viewer, ok := d.CallerID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		// A MALFORMED PATH SEGMENT IS A 400, the same rule messagesHandler and
		// syncHandler apply: the wire's error enum has no member for "your url
		// is unparseable", and `internal` would be a lie about whose fault it is.
		deviceID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is not a valid device id"})
			return
		}

		revoked, err := d.RevokeOwnDevice(r.Context(), viewer, deviceID)
		if err != nil {
			d.Logger.ErrorContext(r.Context(), "device revoke failed",
				"viewer_id", viewer, "device_id", deviceID, "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "device revoke failed"))
			return
		}
		// Logged either way, at the level the outcome deserves: a revocation
		// that changed nothing is worth seeing when somebody is wondering why
		// their phone still works.
		d.Logger.InfoContext(r.Context(), "device revoke",
			"viewer_id", viewer, "device_id", deviceID, "changed", revoked)
		w.WriteHeader(http.StatusNoContent)
	}
}
