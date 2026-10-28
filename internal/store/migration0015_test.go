package store

// CANT-333 (CANT-331 ruling 0 option A) — migration 0015: a nullable
// link_final_host on uploads and attachments, bounded to a bare host.

import (
	"strings"
	"testing"
)

// 0015 is additive: up applies to a database already holding 0014 rows without
// modifying them, down is a plain drop that restores the 0014 constraints, and
// up-down-up lands on the same schema.
func TestMigration0015AppliesAndReverses(t *testing.T) {
	ctx, pool := freshDB(t)
	dropLinkCardsOnCleanup(ctx, t, pool)
	author := mkUser(ctx, t, pool, "author-0015")
	conv := mkGroup(ctx, t, pool, "room", author)
	msg := insertTestMessage(ctx, t, pool, conv, author, 1, 1)

	count := func(q string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	hasCol := func(table string) bool {
		return count(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = 'link_final_host'`, table) == 1
	}
	if !hasCol("uploads") || !hasCol("attachments") {
		t.Fatal("0015 did not add link_final_host to both tables")
	}
	for _, table := range []string{"uploads", "attachments"} {
		var nullable string
		if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name = $1 AND column_name = 'link_final_host'`, table).Scan(&nullable); err != nil || nullable != "YES" {
			t.Errorf("%s.link_final_host nullable = %q (err %v), want YES", table, nullable, err)
		}
	}

	// Down is a plain drop: no refusal.
	if err := MigrateDown(ctx, pool, migrationsAfter(t, "0014")); err != nil {
		t.Fatalf("0015 down: %v", err)
	}
	if hasCol("uploads") || hasCol("attachments") {
		t.Fatal("0015 down left link_final_host behind")
	}
	// The 0014 schema is intact: both link_* constraints are back and still bind.
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, link_url) VALUES (gen_random_uuid(), $1, 'voice', 'k', 0, 'https://x.invalid/')`, author); err == nil {
		t.Error("after 0015 down, a voice upload carrying link_url was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (id, message_id, position, kind, storage_key, filename, width, height, bytes, link_title)
		VALUES (gen_random_uuid(), $1, 0, 'image', 'k', 'f', 1, 1, 1, 't')`, msg); err == nil {
		t.Error("after 0015 down, an image attachment carrying link_title was accepted")
	}

	// Rows written at 0014 survive 0015 up unmodified and read NULL.
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (id, message_id, position, kind, link_url, link_title, link_site_name)
		VALUES (gen_random_uuid(), $1, 0, 'link', 'https://youtu.be/x', 'A title', 'YouTube')`, msg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (id, message_id, position, kind, storage_key, filename, width, height, bytes)
		VALUES (gen_random_uuid(), $1, 1, 'image', 'k', 'f', 1, 1, 1)`, msg); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0015 up again: %v", err)
	}
	if n := count(`SELECT count(*) FROM attachments WHERE message_id = $1 AND link_final_host IS NULL`, msg); n != 2 {
		t.Errorf("%d of 2 pre-existing attachments read NULL link_final_host, want 2", n)
	}
	if n := count(`SELECT count(*) FROM attachments WHERE message_id = $1 AND kind = 'link' AND link_url = 'https://youtu.be/x' AND link_title = 'A title' AND link_site_name = 'YouTube'`, msg); n != 1 {
		t.Error("0015 up modified a pre-existing card")
	}
}

func TestMigration0015FinalHostConstraints(t *testing.T) {
	ctx, pool := freshDB(t)
	dropLinkCardsOnCleanup(ctx, t, pool)
	user := insertTestUser(ctx, t, pool, "constraints-0015")
	conv := mkGroup(ctx, t, pool, "c", user)
	msg := insertTestMessage(ctx, t, pool, conv, user, 1, 1)

	label63 := strings.Repeat("a", 63)
	long254 := strings.Join([]string{label63, label63, label63, strings.Repeat("a", 61)}, ".") // 63*3+61+3 = 253
	tooLong := long254 + "a"                                                                   // 254

	type tc struct {
		name, host string
		ok         bool
	}
	cases := []tc{
		{"a plain host", "www.youtube.com", true},
		{"an IDN as punycode", "xn--bcher-kva.example", true},
		{"an IPv4 literal", "93.184.216.34", true},
		{"a bracketed IPv6 literal", "[2001:db8::1]", true},
		{"underscore and hyphen labels", "a_b.c-d.example", true},
		{"exactly 253 characters", long254, true},
		{"a path (the ticket's example)", "youtube.com/watch", false},
		{"a scheme", "https://youtube.com", false},
		{"a port", "youtube.com:8080", false},
		{"a query", "youtube.com?x=1", false},
		{"uppercase", "YouTube.com", false},
		{"a trailing dot", "youtube.com.", false},
		{"an unbracketed IPv6 literal", "2001:db8::1", false},
		{"a single label", "localhost", false},
		{"a space", "you tube.com", false},
		{"the empty string", "", false},
		{"a 64-character label", label63 + "a.com", false},
		{"254 characters", tooLong, false},
		{"a newline", "youtube.com\n", false},
	}
	for _, c := range cases {
		_, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, bytes, kind, link_url, link_final_host) VALUES (gen_random_uuid(), $1, 0, 'link', 'https://x.invalid/', $2)`, user, c.host)
		if (err == nil) != c.ok {
			t.Errorf("uploads: %s: err = %v, want ok = %v", c.name, err, c.ok)
		}
		_, err = pool.Exec(ctx, `INSERT INTO attachments (id, message_id, position, kind, link_url, link_final_host)
			VALUES (gen_random_uuid(), $1, (SELECT coalesce(max(position), -1) + 1 FROM attachments WHERE message_id = $1), 'link', 'https://x.invalid/', $2)`, msg, c.host)
		if (err == nil) != c.ok {
			t.Errorf("attachments: %s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}

	// NULL is the absent case and is always accepted on a link.
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, bytes, kind, link_url) VALUES (gen_random_uuid(), $1, 0, 'link', 'https://x.invalid/')`, user); err != nil {
		t.Errorf("a link with no final host was refused: %v", err)
	}

	// Only a link may carry it (the extended *_link_columns_only_on_link).
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (id, uploader, bytes, kind, storage_key, link_final_host) VALUES (gen_random_uuid(), $1, 0, 'voice', 'k', 'www.youtube.com')`, user); err == nil {
		t.Error("uploads: a voice row carrying link_final_host was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (id, message_id, position, kind, storage_key, filename, width, height, bytes, link_final_host)
		VALUES (gen_random_uuid(), $1, 99, 'image', 'k', 'f', 1, 1, 1, 'www.youtube.com')`, msg); err == nil {
		t.Error("attachments: an image row carrying link_final_host was accepted")
	}
}
