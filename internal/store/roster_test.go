package store

// CANT-267 — GET /users returns exactly the active persons other than the
// caller: an active person, a deactivated person, a bot and the caller are
// seeded, and only the active person comes back, with its handle.

import (
	"testing"

	"github.com/google/uuid"
)

func TestRosterListsExactlyTheActivePersonsOtherThanTheCaller(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	gone := mkUser(ctx, t, pool, "gone")
	bot := mkUser(ctx, t, pool, "robot")
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	got, err := st.Roster(ctx, ada)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(got) != 1 || got[0].ID != theo || got[0].Handle != "theo" {
		t.Fatalf("roster = %+v, want exactly theo with handle populated", got)
	}

	stranger, err := st.Roster(ctx, uuid.New())
	if err != nil || len(stranger) != 2 {
		t.Fatalf("roster for a stranger = %+v, %v; want ada and theo", stranger, err)
	}
}
