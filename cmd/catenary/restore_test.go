package main

// CANT-102 — CANT-24's criterion 7: obligation 4 against a log that was
// truncated and regrown below the client's cursor, which is what a restore
// from backup looks like from a device.
//
// The same Go client, twice. Correct, it sees `ready.log_seq` below its cursor,
// wipes messages, conversations, users and cursor, bootstraps from 0, and
// holds exactly what a fresh install holds — and still does after the log has
// regrown past its old cursor. With SkipWipe it re-syncs from 0 and keeps its
// store, and its cursor, being monotonic, stays where the old log left it: so
// once the log regrows past that point, every message regrown between the
// discard and the old cursor sits below the cursor and above anything a page
// ever brought it. Those are the regrown messages it ends missing, named one by
// one, beside the phantoms it kept from the log that no longer exists.

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// truncateLog puts the log back to a head of keep, as a restore from a backup
// taken at that head would: every message above it gone, every conversation's
// last_seq and every read mark back inside what remains, the counter at keep.
//
// Refused when a metadata marker sits above keep, because then this would not
// be a restore — a real one would have taken the marker back too.
func truncateLog(ctx context.Context, t *testing.T, pool *pgxpool.Pool, keep int64) {
	t.Helper()
	var marker int64
	if err := pool.QueryRow(ctx, `
		SELECT GREATEST(
		  (SELECT COALESCE(max(metadata_log_seq), 0) FROM conversations),
		  (SELECT COALESCE(max(metadata_log_seq), 0) FROM users),
		  (SELECT COALESCE(max(metadata_log_seq), 0) FROM conversation_members))`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker > keep {
		t.Fatalf("a metadata marker at %d sits above the restore point %d; this truncation would not be a restore", marker, keep)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`DELETE FROM messages WHERE log_seq > $1`, []any{keep}},
		{`UPDATE conversations c
		     SET last_seq = COALESCE((SELECT max(m.seq) FROM messages m WHERE m.conversation_id = c.id), 0)`, nil},
		{`UPDATE conversation_members cm
		     SET read_seq = LEAST(cm.read_seq, c.last_seq)
		    FROM conversations c
		   WHERE c.id = cm.conversation_id`, nil},
		{`UPDATE log_counter SET value = $1 WHERE id = 1`, []any{keep}},
	} {
		if _, err := tx.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("truncate: %v\n%s", err, s.q)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type restoreCheckpoint struct {
	status client.Status
	report client.Report
	// view is every difference from a fresh install's bootstrap at the same
	// moment: messages, conversations and users record by record, and the
	// cursor. Empty is identical.
	view []string
}

type restoreResult struct {
	oldCursor, restoredTo, headAtDiscard int64
	atDiscard, afterRegrowth             restoreCheckpoint
	// regrownBelowOldCursor: messages regrown with log_seq in
	// (headAtDiscard, oldCursor].
	regrownBelowOldCursor []wire.Uuid
	cursorAheadLogged     bool
}

func runRestore(t *testing.T, faults client.Faults) restoreResult {
	t.Helper()
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	rooms := []uuid.UUID{mkGroup(r.ctx, t, r.pool, "A", ada, theo), mkGroup(r.ctx, t, r.pool, "B", ada, theo)}
	commit := func(n int, label string) []store.Sent {
		var out []store.Sent
		for i := 0; i < n; i++ {
			out = append(out, r.commit(rooms[i%2], ada, fmt.Sprintf("%s %d", label, i)))
		}
		return out
	}
	checkpoint := func(c *client.Client, fresh string) restoreCheckpoint {
		snap := c.Snapshot()
		return restoreCheckpoint{
			status: c.Status(),
			report: client.Compare(serverLog(r.ctx, t, r.pool, theo), snap),
			view:   client.SameView(snap, bootstrapView(t, r, theo, fresh)),
		}
	}
	var res restoreResult

	dev := r.enroll(theo, "phone")
	journal := client.NewJournal()
	c := runClient(t, r.ctx, r.base, dev, journal, faults)
	awaitClient(t, c, "the first ready", func() bool {
		s := c.Status()
		return s.Readys >= 1 && s.CaughtUp
	})
	commit(20, "before the restore")
	res.oldCursor = r.head()
	if res.oldCursor != 20 {
		t.Fatalf("head = %d after 20 sends into a fresh log; something else drew from log_counter", res.oldCursor)
	}
	c.CatchUp()
	awaitClient(t, c, "a cursor at head", func() bool {
		s := c.Status()
		return s.CaughtUp && s.Cursor == res.oldCursor && s.Messages == 20
	})
	c.Kill()

	// THE RESTORE, and the log regrows below the cursor while the device is
	// away.
	res.restoredTo = 10
	truncateLog(r.ctx, t, r.pool, res.restoredTo)
	commit(5, "regrown below the old cursor")
	res.headAtDiscard = r.head()
	if res.headAtDiscard >= res.oldCursor {
		t.Fatalf("head %d is not below the cursor %d; the fixture is wrong", res.headAtDiscard, res.oldCursor)
	}

	c = runClient(t, r.ctx, r.base, dev, journal, faults)
	awaitClient(t, c, "the reconnect below the cursor", func() bool {
		s := c.Status()
		return s.Readys >= 1 && s.CaughtUp
	})
	res.atDiscard = checkpoint(c, "a fresh install at the discard")
	c.Kill()

	// And past the old cursor, while it is away again.
	for _, s := range commit(10, "regrown past the old cursor") {
		if s.LogSeq > res.headAtDiscard && s.LogSeq <= res.oldCursor {
			res.regrownBelowOldCursor = append(res.regrownBelowOldCursor, wid(s.ID))
		}
	}
	c = runClient(t, r.ctx, r.base, dev, journal, faults)
	awaitClient(t, c, "the reconnect after the regrowth", func() bool {
		s := c.Status()
		return s.Readys >= 1 && s.CaughtUp
	})
	res.afterRegrowth = checkpoint(c, "a fresh install after the regrowth")

	for _, rec := range r.log.find("session open") {
		if attrOf(rec, "hello_outcome") == string(store.HelloCursorAhead) {
			res.cursorAheadLogged = true
		}
	}
	return res
}

// bootstrapView is a fresh install's store: a new device for the same person,
// an empty journal, one catch-up from 0. It is the server's view as served.
func bootstrapView(t *testing.T, r *rig, user uuid.UUID, name string) client.Snapshot {
	t.Helper()
	c := runClient(t, r.ctx, r.base, r.enroll(user, name), nil, client.Faults{})
	awaitClient(t, c, name+"'s bootstrap", func() bool {
		s := c.Status()
		return s.Readys >= 1 && s.CaughtUp
	})
	snap := c.Snapshot()
	c.Kill()
	return snap
}

// Criterion 7.
func TestALogTruncatedBelowTheCursorIsDiscardedAndBootstrapped(t *testing.T) {
	t.Run("the client wipes and bootstraps from 0", func(t *testing.T) {
		res := runRestore(t, client.Faults{})
		d := res.atDiscard
		if d.status.Discards != 1 || d.status.Wipes != 1 {
			t.Errorf("at the discard: %d discards, %d wipes — want one of each", d.status.Discards, d.status.Wipes)
		}
		if !d.report.Clean() {
			t.Errorf("at the discard: %s\nlost %v\nphantom %v\nseq conflicts %v", d.report, d.report.Lost, d.report.Phantom, d.report.SeqConflicts)
		}
		if len(d.view) != 0 {
			t.Errorf("at the discard the store differs from a fresh install's:\n%v", d.view)
		}
		if !res.cursorAheadLogged {
			t.Error("the hello below the cursor was not logged as cursor_ahead")
		}
		a := res.afterRegrowth
		if a.status.Discards != 0 {
			t.Errorf("after the regrowth: %d discards, want none — the cursor was below head", a.status.Discards)
		}
		if !a.report.Clean() || len(a.view) != 0 {
			t.Errorf("after the regrowth: %s, view differences %v", a.report, a.view)
		}
	})

	t.Run("a client that re-syncs from 0 without wiping ends missing the regrown messages", func(t *testing.T) {
		res := runRestore(t, client.Faults{SkipWipe: true})
		d := res.atDiscard
		if d.status.Discards != 1 || d.status.Wipes != 0 {
			t.Fatalf("at the discard: %d discards, %d wipes — the fault must see the signal and not wipe", d.status.Discards, d.status.Wipes)
		}
		if len(d.report.Phantom) == 0 || len(d.view) == 0 {
			t.Errorf("the kept store matches the server's view at the discard: %s", d.report)
		}
		a := res.afterRegrowth
		want := res.regrownBelowOldCursor
		if int64(len(want)) != res.oldCursor-res.headAtDiscard {
			t.Fatalf("%d messages regrown between head %d and the old cursor %d", len(want), res.headAtDiscard, res.oldCursor)
		}
		if len(a.report.Lost) != len(want) || !containsAll(a.report.Lost, want) {
			t.Errorf("lost %v, want exactly the regrown messages %v: %s", a.report.Lost, want, a.report)
		}
		t.Logf("without the wipe: %s", a.report)
	})
}
