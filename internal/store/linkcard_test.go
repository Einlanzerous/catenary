package store

// CANT-321 (CANT-308 row 2) — a link preview card as a new attachment kind: the
// migration, the single-use handle, and every reader of a NULL storage_key.
//
// A text-only card (a page with a title and no usable image) has no blob, so its
// storage_key is NULL. Each test that mentions "text-only" below is one reader
// of that NULL, named in the plan's schema section.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func linkStore(pool *pgxpool.Pool) *Store {
	return New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(PostgresUploads{})
}

func sptr(s string) *string { return &s }

// dropLinkCardsOnCleanup removes every row of kind link when the test ends, and
// is registered by every test that puts one in the database.
//
// NEEDED BECAUSE THE 0014 DOWN REFUSES OVER A CARD, and the next test's freshDB
// reset is MigrateDown to empty. A card left behind would make that reset fail
// with 0014's own refusal, and every test after it in the package would go down
// with it. Same shape as dropBotHasNoEmailCheck in persons_test.go: registered
// after freshDB's own cleanups, so it runs first, on a still-open pool.
func dropLinkCardsOnCleanup(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM attachments WHERE kind = 'link'`, `DELETE FROM uploads WHERE kind = 'link'`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("cleanup: %s: %v", q, err)
			}
		}
	})
}

// linkHandle mints a ready link handle the way the endpoint (CANT-322) will.
// withImage gives the card a blob key, a thumbnail and dimensions.
func linkHandle(ctx context.Context, t *testing.T, st *Store, uploader uuid.UUID, withImage bool) uuid.UUID {
	t.Helper()
	dropLinkCardsOnCleanup(ctx, t, st.pool)
	u := NewUpload{
		Uploader: uploader, Kind: "link",
		LinkURL: sptr("https://example.invalid/watch?v=abc"), LinkTitle: sptr("A title"),
		LinkDescription: sptr("A description"), LinkSiteName: sptr("Example"),
	}
	if withImage {
		w, h := 1280, 720
		u.StorageKey, u.Bytes = "imagekey0000000000000000000000000000000000000000000000000000abcd", 4096
		u.ThumbKey = sptr("thumbkey000000000000000000000000000000000000000000000000000abcd")
		u.Width, u.Height, u.Placeholder = &w, &h, sptr("LKO2?U%2Tw=w]~RBVZRi};RPxuwH")
	}
	id, err := st.CreateUpload(ctx, u)
	if err != nil {
		t.Fatalf("create link upload: %v", err)
	}
	return id
}

func sendCard(ctx context.Context, st *Store, conv, author, handle uuid.UUID) (Sent, error) {
	text := "look at this"
	return st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: &text,
		Attachments: []NewAttachment{{Kind: "link", UploadID: handle}},
	})
}

// The down REFUSES while a card exists: the rows of kind link in attachments are
// parts of authored messages, and the migrator's one-transaction-per-migration
// means the refusal leaves the schema at 0014 and every row where it was. The
// operator deletes the cards as a typed decision, then the down runs.
func TestMigration0014RefusesToRollBackOverACard(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	author := mkUser(ctx, t, pool, "author-0014r")
	other := mkUser(ctx, t, pool, "other-0014r")
	conv := mkGroup(ctx, t, pool, "room", author, other)

	card := linkHandle(ctx, t, st, author, true)
	sentCard, err := sendCard(ctx, st, conv, author, card)
	if err != nil {
		t.Fatalf("send card: %v", err)
	}
	pending := linkHandle(ctx, t, st, author, false) // an unconsumed link upload

	count := func(q string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	version := func() string {
		var v string
		if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()

	err = MigrateDown(ctx, pool, 1)
	if err == nil {
		t.Fatal("0014 down ran over a link card; it must refuse")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("0014 down error = %v, want a check_violation raised by the migration", err)
	}
	// One card on a message, two link upload rows: the consumed handle's kept
	// row and the pending one. The message names both counts.
	if want := "0014 down refused: 1 link card(s) in attachments and 2 in uploads"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to carry %q", err.Error(), want)
	}
	if !strings.Contains(pgErr.Hint, "DELETE FROM attachments WHERE kind = 'link'") {
		t.Errorf("hint = %q, want the operator told what to run", pgErr.Hint)
	}

	// Nothing moved: schema still at 0014, card and handle still there.
	if got := version(); got != before {
		t.Errorf("schema_migrations max = %s after a refused down, want %s", got, before)
	}
	if n := count(`SELECT count(*) FROM attachments WHERE message_id = $1 AND kind = 'link'`, sentCard.ID); n != 1 {
		t.Errorf("refused down left %d link attachment(s) on the card's message, want 1", n)
	}
	if n := count(`SELECT count(*) FROM uploads WHERE id = $1`, pending); n != 1 {
		t.Error("refused down removed the pending link upload")
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'attachments' AND column_name = 'link_url'`); n != 1 {
		t.Error("refused down dropped link_url")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, link_url) VALUES (gen_random_uuid(), $1, 'link', NULL, 0, 'https://x.invalid/')`, author); err != nil {
		t.Errorf("after a refused down, a text-only link upload was refused: %v", err)
	}

	// The operator's typed decision, then the down goes through and the
	// message survives it.
	if _, err := pool.Exec(ctx, `DELETE FROM attachments WHERE kind = 'link'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM uploads WHERE kind = 'link'`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("0014 down with no cards left: %v", err)
	}
	if n := count(`SELECT count(*) FROM messages WHERE id = $1`, sentCard.ID); n != 1 {
		t.Error("the message that carried a card is gone; only the card may go, and only by hand")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0014 up again: %v", err)
	}
}

// With no card in the database the down is an ordinary reversal: an image
// attachment beside it is untouched, the columns and the NULL go, and up again
// restores them.
func TestMigration0014AppliesAndReverses(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	author := mkUser(ctx, t, pool, "author-0014")
	other := mkUser(ctx, t, pool, "other-0014")
	conv := mkGroup(ctx, t, pool, "room", author, other)

	imgID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, width, height, filename)
		VALUES ($1, $2, 'image', 'k', 1, 1, 1, 'a.png')`, imgID, author); err != nil {
		t.Fatal(err)
	}
	imgSent, err := st.SendMessage(ctx, NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: author,
		Attachments: []NewAttachment{{Kind: "image", UploadID: imgID}}})
	if err != nil {
		t.Fatalf("send image: %v", err)
	}

	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("0014 down: %v", err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM attachments WHERE message_id = $1 AND kind = 'image'`, imgSent.ID); n != 1 {
		t.Error("down touched an image attachment")
	}
	for _, col := range []string{"link_url", "link_title", "link_description", "link_site_name"} {
		if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name IN ('uploads','attachments') AND column_name = $1`, col); n != 0 {
			t.Errorf("down left column %s", col)
		}
	}
	// storage_key is NOT NULL again, and 'link' is refused again.
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes) VALUES (gen_random_uuid(), $1, 'link', NULL, 0)`, author); err == nil {
		t.Error("after down, a link upload was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes) VALUES (gen_random_uuid(), $1, 'voice', NULL, 0)`, author); err == nil {
		t.Error("after down, a NULL storage_key was accepted")
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0014 up again: %v", err)
	}
	dropLinkCardsOnCleanup(ctx, t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, link_url) VALUES (gen_random_uuid(), $1, 'link', NULL, 0, 'https://x.invalid/')`, author); err != nil {
		t.Errorf("after up again, a text-only link upload was refused: %v", err)
	}
}

func TestMigration0014Constraints(t *testing.T) {
	ctx, pool := freshDB(t)
	dropLinkCardsOnCleanup(ctx, t, pool)
	user := insertTestUser(ctx, t, pool, "constraints-0014")
	conv := mkGroup(ctx, t, pool, "c", user)
	msg := insertTestMessage(ctx, t, pool, conv, user, 1, 1)

	type tc struct {
		name, stmt string
		ok         bool
	}
	// uploads
	up := func(name, cols, vals string, ok bool) tc {
		return tc{"uploads: " + name, `INSERT INTO uploads (id, uploader, bytes, ` + cols + `) VALUES (gen_random_uuid(), $1, 0, ` + vals + `)`, ok}
	}
	// attachments
	at := func(name, cols, vals string, ok bool) tc {
		return tc{"attachments: " + name, `INSERT INTO attachments (id, message_id, position, ` + cols + `) VALUES (gen_random_uuid(), $1, (SELECT coalesce(max(position), -1) + 1 FROM attachments WHERE message_id = $1), ` + vals + `)`, ok}
	}
	cases := []tc{
		up("a text-only link", "kind, link_url", `'link', 'https://x.invalid/'`, true),
		up("a link with an image", "kind, storage_key, width, height, link_url", `'link', 'k', 10, 10, 'https://x.invalid/'`, true),
		up("a link with an image and no dimensions", "kind, storage_key, link_url", `'link', 'k', 'https://x.invalid/'`, false),
		up("a link with width and no height", "kind, width, link_url", `'link', 10, 'https://x.invalid/'`, false),
		up("a voice row with no storage_key", "kind", `'voice'`, false),
		up("an image row with no storage_key", "kind, width, height, filename", `'image', 1, 1, 'f'`, false),
		up("a voice row carrying a link url", "kind, storage_key, link_url", `'voice', 'k', 'https://x.invalid/'`, false),
		up("an unknown kind", "kind, storage_key", `'video', 'k'`, false),

		at("a text-only link", "kind, link_url", `'link', 'https://x.invalid/'`, true),
		at("a link with an image", "kind, storage_key, width, height, link_url", `'link', 'k', 10, 10, 'https://x.invalid/'`, true),
		at("a link with no url", "kind, link_title", `'link', 'a title'`, false),
		at("a link with an image and no dimensions", "kind, storage_key, link_url", `'link', 'k', 'https://x.invalid/'`, false),
		at("a link with height and no width", "kind, height, link_url", `'link', 10, 'https://x.invalid/'`, false),
		at("a link with a zero width", "kind, storage_key, width, height, link_url", `'link', 'k', 0, 10, 'https://x.invalid/'`, false),
		at("a voice row with no storage_key", "kind, duration_ms, peaks, transcript_state", `'voice', 1, '{1}', 'pending'`, false),
		at("an image row with no storage_key", "kind, filename, width, height, bytes", `'image', 'f', 1, 1, 1`, false),
		at("an image carrying a link title", "kind, storage_key, filename, width, height, bytes, link_title", `'image', 'k', 'f', 1, 1, 1, 't'`, false),
		at("an unknown kind", "kind, storage_key", `'video', 'k'`, false),
	}
	for _, c := range cases {
		arg := user
		if c.name[:4] == "atta" {
			_, err := pool.Exec(ctx, c.stmt, msg)
			if (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
			}
			continue
		}
		_, err := pool.Exec(ctx, c.stmt, arg)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
}

// A link handle is consumed once, only by its uploader; a second send with it is
// upload_not_found (ruling 6: the stale handle is refused, never resolved twice).
func TestALinkHandleIsConsumedOnceAndOnlyByItsUploader(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	ana := mkUser(ctx, t, pool, "ana-link")
	bob := mkUser(ctx, t, pool, "bob-link")
	conv := mkGroup(ctx, t, pool, "room", ana, bob)

	handle := linkHandle(ctx, t, st, ana, true)

	// Bob cannot take Ana's handle, and the refusal does not consume it.
	if _, err := sendCard(ctx, st, conv, bob, handle); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("another account's link handle: err = %v, want upload_not_found", err)
	}
	var consumed *string
	mustScan(t, pool.QueryRow(ctx, `SELECT consumed_at::text FROM uploads WHERE id = $1`, handle), &consumed)
	if consumed != nil {
		t.Fatal("a refused send consumed the handle")
	}

	if _, err := sendCard(ctx, st, conv, ana, handle); err != nil {
		t.Fatalf("the uploader's send: %v", err)
	}
	if _, err := sendCard(ctx, st, conv, ana, handle); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("a second send with a consumed handle: err = %v, want upload_not_found", err)
	}
	// An unknown handle is indistinguishable.
	if _, err := sendCard(ctx, st, conv, ana, uuid.New()); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("an unknown handle: err = %v", err)
	}
	// And a link handle named as an image is the wrong kind: refused, not served as an image.
	other := linkHandle(ctx, t, st, ana, true)
	text := "x"
	_, err := st.SendMessage(ctx, NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: ana, Text: &text,
		Attachments: []NewAttachment{{Kind: "image", UploadID: other}}})
	if !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("a link handle sent as an image: err = %v", err)
	}
}

// The card's fields survive the send and come back on /sync, with and without an
// image.
func TestACardRoundTripsThroughSync(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	ana := mkUser(ctx, t, pool, "ana-rt")
	bob := mkUser(ctx, t, pool, "bob-rt")
	conv := mkGroup(ctx, t, pool, "room", ana, bob)

	withImg, err := sendCard(ctx, st, conv, ana, linkHandle(ctx, t, st, ana, true))
	if err != nil {
		t.Fatal(err)
	}
	textOnly, err := sendCard(ctx, st, conv, ana, linkHandle(ctx, t, st, ana, false))
	if err != nil {
		t.Fatal(err)
	}

	page, err := st.Sync(ctx, bob, 0, 100)
	if err != nil {
		t.Fatalf("a /sync page holding a text-only card must scan cleanly: %v", err)
	}
	a := page.Attachments[withImg.ID]
	b := page.Attachments[textOnly.ID]
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("attachments = %d and %d, want 1 and 1", len(a), len(b))
	}
	if a[0].Kind != "link" || a[0].StorageKey == "" || a[0].Width == nil || *a[0].Width != 1280 ||
		a[0].Height == nil || *a[0].Height != 720 || a[0].Placeholder == nil ||
		a[0].LinkURL == nil || *a[0].LinkURL != "https://example.invalid/watch?v=abc" ||
		a[0].LinkTitle == nil || *a[0].LinkTitle != "A title" ||
		a[0].LinkDescription == nil || *a[0].LinkDescription != "A description" ||
		a[0].LinkSiteName == nil || *a[0].LinkSiteName != "Example" {
		t.Errorf("card with image = %+v", a[0])
	}
	if a[0].Bytes != nil {
		t.Errorf("a link attachment carries bytes = %d; the count is not a wire field", *a[0].Bytes)
	}
	if b[0].Kind != "link" || b[0].StorageKey != "" || b[0].Width != nil || b[0].LinkURL == nil {
		t.Errorf("text-only card = %+v", b[0])
	}
	// The row really stores NULL, not an empty string.
	var isNull bool
	mustScan(t, pool.QueryRow(ctx, `SELECT storage_key IS NULL FROM attachments WHERE message_id = $1`, textOnly.ID), &isNull)
	if !isNull {
		t.Error("a text-only card stored an empty string, not NULL")
	}

	// A reply whose source is the text-only card also scans (loadReplySources).
	text := "re"
	if _, err := st.SendMessage(ctx, NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: bob,
		Text: &text, ReplyTo: &textOnly.ID}); err != nil {
		t.Fatalf("reply to a text-only card: %v", err)
	}
	if _, err := st.Sync(ctx, ana, 0, 100); err != nil {
		t.Fatalf("a /sync page holding a reply to a text-only card: %v", err)
	}
}

// The resolver consumes a text-only card's handle: RETURNING storage_key scans a
// NULL.
func TestTheResolverConsumesATextOnlyCard(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	ana := mkUser(ctx, t, pool, "ana-res")
	conv := mkGroup(ctx, t, pool, "room", ana)
	h := linkHandle(ctx, t, st, ana, false)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, err := PostgresUploads{}.Resolve(ctx, tx, ana, []NewAttachment{{Kind: "link", UploadID: h}})
	if err != nil {
		t.Fatalf("resolve a text-only card: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "link" || rows[0].StorageKey != "" || rows[0].LinkURL == nil {
		t.Fatalf("rows = %+v", rows)
	}
	_ = conv
}

// GET /media/{key} and ?v=thumb find their row through uploads_storage_key_idx,
// `storage_key = $1`, which never matches NULL. With a text-only card in the
// table, other rows are still found and an unknown key is still not found.
func TestUploadByStorageKeyBesideATextOnlyCard(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	ana := mkUser(ctx, t, pool, "ana-key")
	linkHandle(ctx, t, st, ana, false) // NULL storage_key
	linkHandle(ctx, t, st, ana, true)  // a card with an image
	if _, err := st.CreateUpload(ctx, NewUpload{Uploader: ana, Kind: "voice", StorageKey: "voicekey", Bytes: 3}); err != nil {
		t.Fatal(err)
	}

	kind, thumb, found, err := st.UploadByStorageKey(ctx, "imagekey0000000000000000000000000000000000000000000000000000abcd")
	if err != nil || !found || kind != "link" || thumb == "" {
		t.Errorf("a link card's image: kind %q thumb %q found %v err %v", kind, thumb, found, err)
	}
	if kind, _, found, err := st.UploadByStorageKey(ctx, "voicekey"); err != nil || !found || kind != "voice" {
		t.Errorf("a voice row beside a NULL: kind %q found %v err %v", kind, found, err)
	}
	if _, _, found, err := st.UploadByStorageKey(ctx, "no-such-key"); err != nil || found {
		t.Errorf("an unknown key: found %v err %v, want not found and no error", found, err)
	}
	if _, _, found, err := st.UploadByStorageKey(ctx, ""); err != nil || found {
		t.Errorf("the empty key must not match a NULL storage_key: found %v err %v", found, err)
	}
}

// The CANT-47 section 5 predicate (CANT-307, unbuilt), run as SQL exactly as the
// record words it, against a database that holds a text-only card. It must
// neither error nor report a NULL as a blob. The NOT IN form that goes silently
// wrong with a NULL in the list is run beside it, so the trap is on record and
// the test fails if the fixture ever stops containing a NULL.
func TestTheSection5PredicateWithATextOnlyCard(t *testing.T) {
	ctx, pool := freshDB(t)
	st := linkStore(pool)
	ana := mkUser(ctx, t, pool, "ana-gc")
	conv := mkGroup(ctx, t, pool, "room", ana)

	if _, err := sendCard(ctx, st, conv, ana, linkHandle(ctx, t, st, ana, false)); err != nil {
		t.Fatal(err)
	}
	imgCardKey := "imagekey0000000000000000000000000000000000000000000000000000abcd"
	if _, err := sendCard(ctx, st, conv, ana, linkHandle(ctx, t, st, ana, true)); err != nil {
		t.Fatal(err)
	}
	var nulls int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE storage_key IS NULL`), &nulls)
	if nulls != 1 {
		t.Fatalf("the fixture holds %d NULL storage_key row(s), want exactly 1", nulls)
	}

	// section 5, as written:
	//   live       = unconsumed, or consumed with its storage_key still in attachments
	//   referenced = some attachments.storage_key, or the storage_key or thumb_key of a live row
	const referenced = `
		WITH live AS (
		     SELECT u.* FROM uploads u
		      WHERE u.consumed_at IS NULL
		         OR EXISTS (SELECT 1 FROM attachments a WHERE a.storage_key = u.storage_key)
		), referenced(k) AS (
		     SELECT storage_key FROM attachments
		     UNION SELECT storage_key FROM live
		     UNION SELECT thumb_key FROM live
		)`

	// The candidate blobs: the card's image and thumbnail, and one nobody names.
	candidates := `(VALUES ('` + imgCardKey + `'), ('thumbkey000000000000000000000000000000000000000000000000000abcd'), ('stray-blob'))`

	rows, err := pool.Query(ctx, referenced+`
		SELECT c.k FROM `+candidates+` AS c(k)
		 WHERE NOT EXISTS (SELECT 1 FROM referenced r WHERE r.k = c.k)`)
	if err != nil {
		t.Fatalf("the section 5 predicate errored: %v", err)
	}
	var unreferenced []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		unreferenced = append(unreferenced, k)
	}
	rows.Close()
	if len(unreferenced) != 1 || unreferenced[0] != "stray-blob" {
		t.Errorf("unreferenced = %v, want only the stray blob (a NULL must not be a blob, and the card's image and thumbnail are referenced)", unreferenced)
	}

	// And NULL is not reported as a blob: the live-row test never treats a
	// consumed text-only card as live by accident of NULL = NULL.
	var liveNull int
	mustScan(t, pool.QueryRow(ctx, referenced+` SELECT count(*) FROM live WHERE storage_key IS NULL`), &liveNull)
	if liveNull != 0 {
		t.Errorf("%d consumed text-only card row(s) counted as live", liveNull)
	}

	// The trap, on record: NOT IN over a column that holds a NULL matches nothing.
	var trapped int
	mustScan(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM (VALUES ('stray-blob')) AS c(k)
		 WHERE c.k NOT IN (SELECT storage_key FROM attachments)`), &trapped)
	if trapped != 0 {
		t.Error("NOT IN over attachments.storage_key reported the stray blob; the fixture no longer holds a NULL, so this test proves nothing")
	}
}
