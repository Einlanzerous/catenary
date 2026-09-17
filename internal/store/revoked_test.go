package store

// CANT-30 — the gap re-check's own oracle.
//
// RevokedDevices is what closes the hole a revocation cannot close any other
// way: Postgres queues nothing for a disconnected listener, and unlike the
// message path there is no cursor to replay, so an instance that missed a
// revocation can only find out by asking. These tests are about WHICH devices
// it names, because naming too few leaves a revoked phone streaming and naming
// too many logs out devices nobody revoked.

import (
	"testing"

	"github.com/google/uuid"
)

func TestRevokedDevicesNamesARevokedDeviceAndLeavesTheLiveOnesAlone(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")

	phone := mkDevice(ctx, t, pool, ada, "Pixel 8 Pro")
	laptop := mkDevice(ctx, t, pool, ada, "Framework 13")

	// Nothing is revoked yet, so nothing is named. A re-check that returned a
	// live device would sever a session on every listener reconnect.
	got, err := st.RevokedDevices(ctx, []uuid.UUID{phone, laptop})
	if err != nil {
		t.Fatalf("revoked devices: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("%v named as revoked while both devices are live", got)
	}

	if _, err := st.RevokeDevice(ctx, phone); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err = st.RevokedDevices(ctx, []uuid.UUID{phone, laptop})
	if err != nil {
		t.Fatalf("revoked devices: %v", err)
	}
	if len(got) != 1 || got[0] != phone {
		t.Errorf("revoked devices = %v, want exactly the revoked phone %s — the other device "+
			"belongs to the same person and was not revoked", got, phone)
	}
}

// THE CASE THE DEVICE COLUMN ALONE WOULD MISS. A deactivated account's devices
// are never individually revoked — Purser disables the account — so a re-check
// that read only devices.revoked_at would leave a disabled person's socket
// streaming. RevocationPayload has carried a user subject since CANT-28 for
// exactly this, and the query joins users for the same reason.
func TestRevokedDevicesNamesALiveDeviceWhoseAccountWasDeactivated(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	gone := mkUser(ctx, t, pool, "gone")
	ada := mkUser(ctx, t, pool, "ada")

	disabled := mkDevice(ctx, t, pool, gone, "Pixel")
	live := mkDevice(ctx, t, pool, ada, "Framework")

	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	// The device itself is deliberately still live, so only users.deactivated_at
	// can be doing the work.
	var revoked *string
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at::text FROM devices WHERE id = $1`, disabled).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != nil {
		t.Fatal("precondition: the device must be un-revoked, or this proves nothing")
	}

	got, err := st.RevokedDevices(ctx, []uuid.UUID{disabled, live})
	if err != nil {
		t.Fatalf("revoked devices: %v", err)
	}
	if len(got) != 1 || got[0] != disabled {
		t.Errorf("revoked devices = %v, want the deactivated account's device %s. A re-check "+
			"reading only devices.revoked_at leaves a disabled person's socket streaming", got, disabled)
	}
}

func TestRevokedDevicesAsksNothingForAnEmptySetAndIgnoresUnknownIds(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	// An instance holding no sessions. Not an error, and not a round trip.
	got, err := st.RevokedDevices(ctx, nil)
	if err != nil {
		t.Fatalf("empty set: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty set named %v", got)
	}

	// A device id that is not in the table at all is simply absent from the
	// answer rather than an error: the hub asks about what it holds, and a row
	// can be gone for reasons that are not this query's business.
	got, err = st.RevokedDevices(ctx, []uuid.UUID{uuid.New()})
	if err != nil {
		t.Fatalf("unknown id: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an unknown device id was named as revoked: %v", got)
	}
}
