package main

// CANT-165 — the hard requirement, proved without a database: restoreprobe
// refuses to run at all against a database that was not explicitly
// acknowledged as scratch. No build tag and no CATENARY_TEST_DATABASE_URL
// gate, unlike restoreprobe_test.go's smoke run — checkScratchDatabase is
// pure parsing over -db-url, so this runs every time `go test ./...` does.

import (
	"strings"
	"testing"
)

func TestCheckScratchDatabase(t *testing.T) {
	cases := []struct {
		name      string
		dbURL     string
		scratchOk string
		wantErr   bool
	}{
		{"matching acknowledgement passes", "postgres://u:p@host:5432/catenary_test?sslmode=disable", "catenary_test", false},
		{"a different scratch name still refuses without its own ack", "postgres://u:p@host:5432/catenary_drill?sslmode=disable", "catenary_test", true},
		{"no acknowledgement at all refuses", "postgres://u:p@host:5432/catenary_test?sslmode=disable", "", true},
		{"production database refused even with a matching ack", "postgres://u:p@host:5432/catenary?sslmode=disable", "catenary", true},
		{"the postgres maintenance database refused even with a matching ack", "postgres://u:p@host:5432/postgres?sslmode=disable", "postgres", true},
		{"no database name at all refuses", "postgres://u:p@host:5432/?sslmode=disable", "", true},
		{"an unparseable URL refuses", "not a url at all", "whatever", true},
		// pgx's own ParseConfigError redacts the password before formatting
		// (pgconn.ParseConfigError.Error() calls redactPW) — this is the one
		// case that actually exercises that path, since every OTHER refusal
		// below is built from the parsed-out database name and never from
		// dbURL itself.
		{"a URL that fails to parse still never leaks its password", "postgres://user:sup3rsecret@host:not-a-number/db", "whatever", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, err := checkScratchDatabase(c.dbURL, c.scratchOk)
			if (err != nil) != c.wantErr {
				t.Fatalf("checkScratchDatabase(%q, %q) = (%q, %v), wantErr %t", c.dbURL, c.scratchOk, name, err, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "sup3rsecret") {
				t.Errorf("the refusal's error text carries the connection string's password: %v", err)
			}
		})
	}
}
