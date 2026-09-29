//go:build restoreprobesmoke

package main

// TestSmokeRestoreProbe is CANT-165's Done-when made a step in verify.sh: a
// real subprocess, a real Postgres, a real truncateLog-shaped restore, and
// restoreprobe run against it exactly as CANT-167's drill will run it.
//
// BEHIND A BUILD TAG, but NOT because a real subprocess is otherwise absent
// from the ordinary sweep — restoreprobe_cli_test.go's own
// TestRestoreProbeCmdAgainstALocalServer already starts one and runs a full
// passing probe on every DB-lane `go test ./...`. What this file pays for
// ONCE, from its own named verify.sh step, is the truncated-restore and
// broken-counter SCENARIOS specifically: seeding thirty messages, truncating
// the log, and deliberately breaking the counter, on top of the subprocess
// the ordinary sweep already starts for a fresh database.
//
// checkScratchDatabase itself — the hard safety requirement — is proved
// separately and unconditionally in restoreprobe_safety_test.go, which needs
// no database and so carries no build tag.

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/store"
)

func TestSmokeRestoreProbe(t *testing.T) {
	dbURL := soakDBFixture(t)
	h := startLocalServer(t, dbURL)
	ctx := context.Background()

	dbCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	dbName := dbCfg.ConnConfig.Database

	// Seed: one author, one group, thirty messages — so log_counter and the
	// room's last_seq both start well above zero before the "restore".
	author := uuid.New()
	if err := insertUser(ctx, h.pool, author, "restoreprobe-seed-author", "Seed Author"); err != nil {
		t.Fatalf("seed author: %v", err)
	}
	const roomName = "restoreprobe smoke room"
	room := mkGroupForProbe(ctx, t, h.pool, roomName, author)
	for i := 0; i < 30; i++ {
		text := fmt.Sprintf("seed message %d", i)
		if _, err := h.store.SendMessage(ctx, store.NewMessage{
			ClientID: uuid.New(), ConversationID: room, AuthorID: author, Text: &text,
		}); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}

	// The restore: everything above log_seq 10 is gone, exactly as a backup
	// taken at that head and restored now would leave it.
	truncateLogForTest(ctx, t, h.pool, 10)

	t.Run("the discard and the correct ordinals", func(t *testing.T) {
		res := runRestoreProbe(context.Background(), RestoreProbeConfig{
			DBURL: dbURL, BaseURL: h.baseURL, ScratchOk: dbName,
			Conversation: roomName, Ahead: 50,
			CredFile:     filepath.Join(t.TempDir(), "creds.json"),
			AwaitTimeout: 20 * time.Second,
			Logger:       slog.New(slog.DiscardHandler),
		})
		t.Logf("report: %+v", res.Report)
		if res.Verdict != VerdictPass {
			t.Fatalf("verdict = %s, want %s", res.Verdict, VerdictPass)
		}
		if res.Report.Discards < 1 || res.Report.Wipes < 1 {
			t.Errorf("discards=%d wipes=%d, want at least one of each — the dialed cursor (%d) was set above the restored head (%d)",
				res.Report.Discards, res.Report.Wipes, res.Report.CursorDialed, res.Report.HeadBeforeDial)
		}
		if !res.Report.Sent {
			t.Fatal("the send never reached an answer")
		}
		if !res.Report.OrdinalsMatch {
			t.Errorf("ordinals did not match: ack log_seq/seq = %d/%d, want %d/%d",
				res.Report.AckLogSeq, res.Report.AckSeq, res.Report.WantLogSeq, res.Report.WantSeq)
		}
	})

	t.Run("a deliberately broken counter fails with a unique violation, reported as a failure", func(t *testing.T) {
		// Set BELOW max(log_seq): the next draw collides with a message
		// that already holds it — Invariant 1's bigserial failure, on
		// purpose, so restoreprobe has to actually notice rather than
		// assume a clean run.
		if _, err := h.pool.Exec(ctx,
			`UPDATE log_counter SET value = (SELECT max(log_seq) FROM messages) - 1 WHERE id = 1`); err != nil {
			t.Fatalf("break the counter: %v", err)
		}

		res := runRestoreProbe(context.Background(), RestoreProbeConfig{
			DBURL: dbURL, BaseURL: h.baseURL, ScratchOk: dbName,
			Conversation: roomName, Ahead: 5,
			CredFile:     filepath.Join(t.TempDir(), "creds.json"),
			AwaitTimeout: 20 * time.Second,
			Logger:       slog.New(slog.DiscardHandler),
		})
		t.Logf("report: %+v", res.Report)
		if res.Verdict != VerdictServerFailure {
			t.Fatalf("verdict = %s, want %s (a unique violation reported as a failure)", res.Verdict, VerdictServerFailure)
		}
		if res.Report.Sent {
			t.Error("Sent = true on a run whose send should have been refused")
		}
		if res.Report.SendError == "" {
			t.Error("no send error was recorded — want the refusal's wire code and message")
		}
	})
}
