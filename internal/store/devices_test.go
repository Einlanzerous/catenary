package store

// CANT-117 — the device list and the scoped revoke.
//
// THE SECURITY-CRITICAL ONE IS OWNERSHIP, and it is why the scoping lives in
// the store rather than in a handler. `RevokeDevice` takes a device id alone
// and is correct for R6's Deprovision, which revokes somebody else's devices on
// purpose. A self-service route over that shape would let anyone revoke a
// stranger's phone with nothing but its id, so `RevokeOwnDevice` carries the
// user in its WHERE clause and these tests are what hold it there.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDevicesForReturnsOnlyTheCallersOwnDevicesOldestFirst(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")

	phone := mkDevice(ctx, t, pool, ada, "Pixel 8 Pro")
	laptop := mkDevice(ctx, t, pool, ada, "Framework 13")
	theirs := mkDevice(ctx, t, pool, theo, "Theo's phone")

	// Deterministic ordering needs distinct created_at values; mkDevice inserts
	// them within the same millisecond otherwise.
	if _, err := pool.Exec(ctx,
		`UPDATE devices SET created_at = now() - interval '1 day' WHERE id = $1`, phone); err != nil {
		t.Fatal(err)
	}

	got, err := st.DevicesFor(ctx, ada)
	if err != nil {
		t.Fatalf("devices for: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d devices, want 2 — %v", len(got), got)
	}
	if got[0].ID != phone || got[1].ID != laptop {
		t.Errorf("order = %s, %s; want oldest first (%s, %s)", got[0].ID, got[1].ID, phone, laptop)
	}
	for _, d := range got {
		if d.ID == theirs {
			t.Fatal("another account's device is in this list. The list is scoped by the caller's " +
				"own id and nothing in a request chooses it")
		}
		if d.RevokedAt != nil {
			t.Errorf("%s is live and carries revoked_at %v", d.ID, d.RevokedAt)
		}
		if d.Name == "" {
			t.Errorf("%s has no name — a revocation list is unusable if the rows do not say "+
				"which phone they are", d.ID)
		}
	}
}

// A device row is never deleted, so a revoked one stays in the list with the
// moment it was revoked. Hiding it would discard the history the column exists
// to keep.
func TestDevicesForKeepsRevokedDevicesAndDatesThem(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	phone := mkDevice(ctx, t, pool, ada, "Pixel 8 Pro")

	if _, err := st.RevokeDevice(ctx, phone); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got, err := st.DevicesFor(ctx, ada)
	if err != nil {
		t.Fatalf("devices for: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d devices after revoking the only one, want 1 — a revoked device row is never "+
			"deleted, and dropping it from the list discards what the column is for", len(got))
	}
	if got[0].RevokedAt == nil {
		t.Error("the revoked device carries no revoked_at; absent means live on the wire, so this " +
			"would render as a working phone")
	}
}

// An account with no devices — a bot, or a person before their first enrollment
// — is an empty list rather than an error.
func TestDevicesForAnAccountWithNoDevicesIsEmptyAndNotAnError(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot := mkUser(ctx, t, pool, "argosy-bot")
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	got, err := st.DevicesFor(ctx, bot)
	if err != nil {
		t.Fatalf("a bot's device list errored: %v — a caller with nothing to list is not a caller "+
			"who may not look", err)
	}
	if len(got) != 0 {
		t.Errorf("%d devices for a bot, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// The scoped revoke

func TestRevokeOwnDeviceRevokesYourOwnAndPublishesIt(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	phone := mkDevice(ctx, t, pool, ada, "Pixel 8 Pro")

	got := revocationsDuring(ctx, t, pool, func() {
		revoked, err := st.RevokeOwnDevice(ctx, ada, phone)
		if err != nil {
			t.Errorf("revoke own device: %v", err)
		}
		if !revoked {
			t.Error("revoking a live device of your own reported no change")
		}
	})
	if len(got) != 1 {
		t.Fatalf("%d revocations published, want 1 — without one the device keeps its live socket "+
			"until it happens to drop", len(got))
	}
	if got[0].DeviceID == nil || *got[0].DeviceID != phone {
		t.Errorf("the revocation names %v, want %s", got[0].DeviceID, phone)
	}

	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM devices WHERE id = $1 AND revoked_at IS NOT NULL`, phone); n != 1 {
		t.Error("devices.revoked_at was not written")
	}
}

// THE ONE THAT MATTERS. A device id is all anyone would need if the ownership
// predicate were not in the write.
func TestRevokeOwnDeviceCannotTouchAnotherAccountsDevice(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	theirs := mkDevice(ctx, t, pool, theo, "Theo's phone")

	got := revocationsDuring(ctx, t, pool, func() {
		revoked, err := st.RevokeOwnDevice(ctx, ada, theirs)
		if err != nil {
			t.Errorf("revoke: %v", err)
		}
		if revoked {
			t.Error("ada revoked theo's device and was told it worked")
		}
	})

	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM devices WHERE id = $1 AND revoked_at IS NULL`, theirs); n != 1 {
		t.Fatal("ANOTHER ACCOUNT'S DEVICE WAS REVOKED. The ownership predicate is in the WHERE " +
			"clause precisely so a device id alone is not enough, and a self-service route over " +
			"the unscoped RevokeDevice would hand anyone a stranger's phone")
	}
	if len(got) != 0 {
		t.Errorf("%d revocations published for a device that was not revoked, want 0 — a "+
			"notification with no cause would sever a session nobody revoked", len(got))
	}
}

// Not yours, unknown and already revoked are ONE answer, so the route cannot be
// used to learn which device ids exist on other accounts.
func TestRevokeOwnDeviceAnswersTheSameForNotYoursUnknownAndAlreadyRevoked(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	mine := mkDevice(ctx, t, pool, ada, "Pixel 8 Pro")
	theirs := mkDevice(ctx, t, pool, theo, "Theo's phone")

	if revoked, err := st.RevokeOwnDevice(ctx, ada, mine); err != nil || !revoked {
		t.Fatalf("precondition: revoking my own live device = (%v, %v)", revoked, err)
	}

	for _, tc := range []struct {
		name   string
		device uuid.UUID
	}{
		{"already revoked", mine},
		{"not yours", theirs},
		{"unknown", uuid.New()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revoked, err := st.RevokeOwnDevice(ctx, ada, tc.device)
			if err != nil {
				t.Errorf("err = %v, want nil — all three are \"nothing changed\", not failures", err)
			}
			if revoked {
				t.Errorf("reported a change for %s", tc.name)
			}
		})
	}
}

// A device revoked by its owner is refused on its next request, which is what
// makes the button mean anything.
func TestARevokedOwnDeviceIsRefusedOnItsNextRequest(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if revoked, err := st.RevokeOwnDevice(ctx, e.UserID, e.DeviceID); err != nil || !revoked {
		t.Fatalf("revoke own device = (%v, %v)", revoked, err)
	}
	if _, err := st.Authenticate(ctx, e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a device revoked by its owner still authenticates: err = %v", err)
	}
	// And it cannot rotate its way back in either.
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a device revoked by its owner still rotates: err = %v", err)
	}
}

// The row keeps the moment, which is what the list renders.
func TestRevokeOwnDeviceDatesTheRevocation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	phone := mkDevice(ctx, t, pool, ada, "Pixel 8 Pro")

	before := time.Now().Add(-time.Minute)
	if _, err := st.RevokeOwnDevice(ctx, ada, phone); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err := st.DevicesFor(ctx, ada)
	if err != nil || len(got) != 1 {
		t.Fatalf("devices for = (%v, %v)", got, err)
	}
	if got[0].RevokedAt == nil || !got[0].RevokedAt.After(before) {
		t.Errorf("revoked_at = %v, want a moment after %v", got[0].RevokedAt, before)
	}
}
