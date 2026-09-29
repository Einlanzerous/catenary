package main

// CANT-165 — `soakrig restoreprobe`: the work-breakdown row 1 CANT-68's
// drill needs before it can probe a served, restored instance at all.
// cmd/catenary/restore_test.go and internal/client already answer "does a
// client above head get cursor_ahead and discard, and does the next insert
// draw the right ordinals" — but only inside `go test`, against a server
// and a database that test built for itself. CANT-167's drill has neither:
// a real restored database and a real `catenary serve` an operator already
// started. This file is the same two questions, asked from outside.
//
// BUILT ON PROVISIONONE'S PATH, NOT A SECOND ONE. The one account this needs
// is created exactly the way `provision` and `soak` already create theirs —
// insertUser, then mintAndRedeem's real POST /enroll — which is CANT-109's
// direct-insert-and-real-enroll path reused here rather than reimplemented a
// third time. See provision.go and provisionone.go.
//
// SAFETY IS THE POINT OF THIS TOOL. It refuses to run at all against a
// database that was not explicitly acknowledged as scratch
// (checkScratchDatabase), a credential never reaches stdout, stderr or the
// logger, and everything it reports is a count, an ordinal or a short error
// code — never a message body, a handle, a token or a raw database error.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// RestoreProbeConfig is everything one restoreprobe run needs.
type RestoreProbeConfig struct {
	DBURL   string
	BaseURL string

	// ScratchOk must repeat, EXACTLY, the database name pgx itself parses
	// out of DBURL — the explicit acknowledgement CLAUDE.md's safety rule
	// requires. See checkScratchDatabase for the two names refused
	// regardless of a match.
	ScratchOk string

	// Conversation is the name of the ALREADY-EXISTING conversation the new
	// account joins. Refused unless it resolves to exactly one row.
	Conversation string

	// Handle is the new account's handle. Empty generates one: this is a
	// one-off probe account nobody needs to type again.
	Handle string

	// Ahead is N: the cursor dialed with is log_counter's value — read once,
	// before anything else — plus Ahead. At least 1, because a cursor equal
	// to head is an ordinary reconnect and not the cursor_ahead shape this
	// tool exists to drive.
	Ahead int64

	// CredFile is where the new device's credential is written, at 0600.
	// Required. Never printed, here or anywhere downstream of writeCredentials.
	CredFile string

	// AwaitTimeout bounds the wait for the client to reach ready and caught
	// up. Zero means 30s.
	AwaitTimeout time.Duration

	Headers http.Header
	Logger  *slog.Logger
}

func (c *RestoreProbeConfig) validate() error {
	switch {
	case c.DBURL == "":
		return errors.New("a database URL is required (-db-url)")
	case c.BaseURL == "":
		return errors.New("-base-url is required")
	case c.Conversation == "":
		return errors.New("-conversation is required")
	case c.CredFile == "":
		return errors.New("-cred-file is required")
	case c.Ahead < 1:
		return errors.New("-ahead must be at least 1 — a cursor equal to head is not cursor_ahead")
	}
	if c.AwaitTimeout <= 0 {
		c.AwaitTimeout = 30 * time.Second
	}
	return nil
}

func (c *RestoreProbeConfig) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// RestoreProbeReport is CANT-165's Done-when in struct form: counts and
// ordinals only. Nothing here is a message body, a handle or anything
// secret, and SendError carries only the wire code and the server's own
// (already generic) refusal message — never the underlying cause.
type RestoreProbeReport struct {
	HeadBeforeDial int64
	CursorDialed   int64

	Discards int
	Wipes    int

	CounterBeforeSend int64
	LastSeqBeforeSend int64
	WantLogSeq        int64
	WantSeq           int64

	Sent          bool
	AckLogSeq     int64
	AckSeq        int64
	OrdinalsMatch bool

	SendError string
}

// RestoreProbeResult is what one call to runRestoreProbe returns.
type RestoreProbeResult struct {
	Report  RestoreProbeReport
	Verdict Verdict
}

// checkScratchDatabase is the hard requirement: refuse a database whose name
// was not explicitly acknowledged. name comes from pgx's OWN parse of dbURL
// — never from splitting the URL string by hand, which a stray slash or an
// unusual DSN shape could fool — so this runs before anything else touches
// the network.
//
// TWO NAMES ARE REFUSED REGARDLESS OF A MATCHING scratchOk: "catenary" is
// the production database and "postgres" is the cluster's own maintenance
// database. An acknowledgement that happens to repeat either back is not
// evidence the caller meant to hit it. Every other name is accepted, and
// only once scratchOk repeats it exactly — a boolean flag would let a
// script carry "-scratch-ok" unconditionally and stop meaning anything at
// all, which is the one failure mode this check exists to close.
func checkScratchDatabase(dbURL, scratchOk string) (name string, err error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		// pgx's own ParseConfigError redacts the password before formatting
		// — see pgconn.ParseConfigError.Error() — so this is safe to
		// propagate and to print.
		return "", fmt.Errorf("parse -db-url: %w", err)
	}
	name = cfg.ConnConfig.Database
	switch name {
	case "", "catenary", "postgres":
		return "", fmt.Errorf("refusing to run against database %q: this name is never a scratch database, whatever -scratch-ok says", name)
	}
	if scratchOk != name {
		return "", fmt.Errorf("refusing to run against database %q without an explicit scratch acknowledgement: -scratch-ok must repeat this exact name, got %q", name, scratchOk)
	}
	return name, nil
}

// runRestoreProbe is CANT-165's Done-when, end to end: provision one account
// into an existing conversation, dial with a cursor set above head, observe
// the discard, then send one message and check its ack's ordinals against
// what the database held immediately before the send.
func runRestoreProbe(ctx context.Context, cfg RestoreProbeConfig) RestoreProbeResult {
	var rep RestoreProbeReport
	log := cfg.logger()

	if err := cfg.validate(); err != nil {
		log.Warn("restoreprobe: config", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	if _, err := checkScratchDatabase(cfg.DBURL, cfg.ScratchOk); err != nil {
		log.Warn("restoreprobe: refused", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	pool, err := store.ConnectWithRetry(ctx, cfg.DBURL, 30*time.Second)
	if err != nil {
		log.Warn("restoreprobe: connect to the database", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	defer pool.Close()
	st := store.New(pool, store.DefaultLimits(), log)

	head, err := st.Head(ctx)
	if err != nil {
		log.Warn("restoreprobe: read the counter", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	rep.HeadBeforeDial = head
	rep.CursorDialed = head + cfg.Ahead

	convID, err := findConversationByName(ctx, pool, cfg.Conversation)
	if err != nil {
		log.Warn("restoreprobe: find the conversation", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	handle := cfg.Handle
	if handle == "" {
		handle = "restoreprobe-" + uuid.NewString()[:8]
	}
	userID := uuid.New()
	if err := insertUser(ctx, pool, userID, handle, handle); err != nil {
		if isUniqueViolation(err) {
			log.Warn("restoreprobe: handle already exists", "error", err)
		} else {
			log.Warn("restoreprobe: create the account", "error", err)
		}
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`,
		convID, userID); err != nil {
		log.Warn("restoreprobe: join the conversation", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	enrolled, err := mintAndRedeem(ctx, st, cfg.BaseURL, cfg.Headers, userID, handle)
	if err != nil {
		log.Warn("restoreprobe: enroll the device", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	cred, err := credentialsFromEnrollment(enrolled)
	if err != nil {
		log.Warn("restoreprobe: read back the enrollment", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	if err := writeCredentials(cfg.CredFile, cred); err != nil {
		log.Warn("restoreprobe: write credentials", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	j := client.NewJournal()
	clientCred, err := client.CredentialFromEnroll(enrolled)
	if err == nil {
		err = j.Enroll(clientCred)
	}
	if err == nil {
		err = j.SeedCursor(rep.CursorDialed)
	}
	if err != nil {
		log.Warn("restoreprobe: seed the journal", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	c, err := client.New(client.Config{
		BaseURL: cfg.BaseURL, ClientInfo: "cant-165-restoreprobe", Journal: j,
		ExtraHeaders: cfg.Headers, Logger: log,
	})
	if err != nil {
		log.Warn("restoreprobe: construct the client", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	defer func() { c.Kill(); <-done }()

	actx, acancel := context.WithTimeout(ctx, cfg.AwaitTimeout)
	awaitErr := c.Await(actx, func() bool { s := c.Status(); return s.Ready && s.CaughtUp })
	acancel()
	if awaitErr != nil {
		log.Warn("restoreprobe: the client never reached ready and caught up",
			"error", awaitErr, "status", c.Status())
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}

	status := c.Status()
	rep.Discards, rep.Wipes = status.Discards, status.Wipes
	if rep.Discards < 1 || rep.Wipes < 1 {
		// A cursor dialed at head + Ahead (Ahead >= 1) can only read BELOW
		// the server's own head at hello time if the head advanced past it
		// between our read and the dial — which nothing but this probe's own
		// send does on a scratch database. Absent that, this is obligation 4
		// itself not firing: a real finding, not an inconclusive run.
		log.Warn("restoreprobe: no discard observed where one was expected",
			"discards", rep.Discards, "wipes", rep.Wipes,
			"head_before_dial", rep.HeadBeforeDial, "cursor_dialed", rep.CursorDialed)
		return RestoreProbeResult{Report: rep, Verdict: VerdictServerFailure}
	}

	counter, err := st.Head(ctx)
	if err != nil {
		log.Warn("restoreprobe: re-read the counter", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	lastSeq, err := readLastSeq(ctx, pool, convID)
	if err != nil {
		log.Warn("restoreprobe: read last_seq", "error", err)
		return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
	}
	rep.CounterBeforeSend, rep.LastSeqBeforeSend = counter, lastSeq
	rep.WantLogSeq, rep.WantSeq = counter+1, lastSeq+1

	sctx, scancel := context.WithTimeout(ctx, 15*time.Second)
	text := "restoreprobe"
	ack, sendErr := c.Send(sctx, wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(convID), Text: &text})
	scancel()
	if sendErr != nil {
		if !sendWasRefused(sendErr) {
			// A timeout, a dropped socket — the send never got an answer at
			// all, so nothing was learned about the ordinals either way.
			log.Warn("restoreprobe: send did not reach an answer", "error", sendErr)
			return RestoreProbeResult{Report: rep, Verdict: VerdictHarnessFailure}
		}
		var refused *client.SendError
		errors.As(sendErr, &refused)
		rep.SendError = fmt.Sprintf("%s: %s", refused.Frame.Code, refused.Frame.Message)
		log.Warn("restoreprobe: send refused", "code", refused.Frame.Code)
		return RestoreProbeResult{Report: rep, Verdict: VerdictServerFailure}
	}

	rep.Sent = true
	rep.AckLogSeq, rep.AckSeq = ack.LogSeq, ack.Seq
	rep.OrdinalsMatch = ack.LogSeq == rep.WantLogSeq && ack.Seq == rep.WantSeq
	if !rep.OrdinalsMatch {
		log.Warn("restoreprobe: the ack's ordinals do not match what the database held before the send",
			"ack_log_seq", ack.LogSeq, "want_log_seq", rep.WantLogSeq,
			"ack_seq", ack.Seq, "want_seq", rep.WantSeq)
		return RestoreProbeResult{Report: rep, Verdict: VerdictServerFailure}
	}

	return RestoreProbeResult{Report: rep, Verdict: VerdictPass}
}

// findConversationByName refuses unless exactly one conversation carries
// name — zero is a probe pointed at the wrong database or run before
// seeding, and more than one is a name restoreprobe cannot disambiguate.
func findConversationByName(ctx context.Context, pool *pgxpool.Pool, name string) (uuid.UUID, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM conversations WHERE name = $1 LIMIT 2`, name)
	if err != nil {
		return uuid.Nil, fmt.Errorf("query conversation %q: %w", name, err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return uuid.Nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	switch len(ids) {
	case 0:
		return uuid.Nil, fmt.Errorf("no conversation is named %q", name)
	case 1:
		return ids[0], nil
	default:
		return uuid.Nil, fmt.Errorf("more than one conversation is named %q; restoreprobe needs exactly one", name)
	}
}

func readLastSeq(ctx context.Context, pool *pgxpool.Pool, convID uuid.UUID) (int64, error) {
	var lastSeq int64
	if err := pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, convID).Scan(&lastSeq); err != nil {
		return 0, fmt.Errorf("read last_seq for %s: %w", convID, err)
	}
	return lastSeq, nil
}

// printRestoreProbeReport is the whole of what this command puts on stdout:
// counts and ordinals, and — only on a refusal — the wire code and message
// the server itself already sends every client. Never a handle, a token or
// a raw database error.
func printRestoreProbeReport(w io.Writer, rep RestoreProbeReport) {
	fmt.Fprintf(w, "head before dial:      %d\n", rep.HeadBeforeDial)
	fmt.Fprintf(w, "cursor dialed:         %d\n", rep.CursorDialed)
	fmt.Fprintf(w, "discards / wipes:      %d / %d\n", rep.Discards, rep.Wipes)
	fmt.Fprintf(w, "counter before send:   %d\n", rep.CounterBeforeSend)
	fmt.Fprintf(w, "last_seq before send:  %d\n", rep.LastSeqBeforeSend)
	fmt.Fprintf(w, "want log_seq / seq:    %d / %d\n", rep.WantLogSeq, rep.WantSeq)
	if rep.Sent {
		fmt.Fprintf(w, "ack log_seq / seq:     %d / %d\n", rep.AckLogSeq, rep.AckSeq)
		fmt.Fprintf(w, "ordinals match:        %t\n", rep.OrdinalsMatch)
	}
	if rep.SendError != "" {
		fmt.Fprintf(w, "send error:            %s\n", rep.SendError)
	}
}
