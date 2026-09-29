package main

// CANT-165 — the CLI wrapper, against a real local server: a credential
// never reaches stdout or stderr, the credential file lands at 0600, the
// exit code matches the verdict, and a database named without a matching
// -scratch-ok is refused before anything else runs. Gated on
// CATENARY_TEST_DATABASE_URL, the same convention provisionone_test.go and
// broken_test.go use — no build tag, so this runs every time `go test ./...`
// does.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRestoreProbeCmdAgainstALocalServer(t *testing.T) {
	dbURL := soakDBFixture(t)
	h := startLocalServer(t, dbURL)
	ctx := context.Background()

	dbCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	dbName := dbCfg.ConnConfig.Database

	author := uuid.New()
	if err := insertUser(ctx, h.pool, author, "restoreprobe-cli-seed-author", "Seed Author"); err != nil {
		t.Fatalf("seed author: %v", err)
	}
	const roomName = "restoreprobe cli test room"
	mkGroupForProbe(ctx, t, h.pool, roomName, author)

	t.Run("refuses a database without a matching -scratch-ok, before touching anything", func(t *testing.T) {
		credFile := filepath.Join(t.TempDir(), "creds.json")
		args := []string{
			"-db-url", dbURL, "-base-url", h.baseURL,
			"-scratch-ok", "some-other-name",
			"-conversation", roomName, "-cred-file", credFile,
			"-quiet",
		}
		var code int
		stdout, stderr := captureStdIO(t, func() { code = runRestoreProbeCmd(args) })
		if code != exitUsage {
			t.Fatalf("runRestoreProbeCmd = %d, want %d (exitUsage); stdout=%q stderr=%q", code, exitUsage, stdout, stderr)
		}
		if stdout != "" {
			t.Errorf("a refused run still wrote a report to stdout: %q", stdout)
		}
		if _, err := os.Stat(credFile); err == nil {
			t.Error("a credential file was written for a run that should have been refused before provisioning anything")
		}
		var userCount int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&userCount); err != nil {
			t.Fatal(err)
		}
		if userCount != 1 {
			t.Errorf("users = %d, want exactly 1 (only the seed author) — a refused run must not provision", userCount)
		}
	})

	t.Run("passes, writes a 0600 credential file, and never prints a secret", func(t *testing.T) {
		credFile := filepath.Join(t.TempDir(), "creds.json")
		// NO -quiet HERE, deliberately: -quiet leaves cfg.Logger nil, which
		// falls back to slog.DiscardHandler and means nothing this run logs
		// — the probe's own log.Warn calls, or internal/client's "ready"
		// line, which carries the SAME logger — ever reaches stderr. The
		// stderr assertion below is only a real check on the logger path
		// with the logger actually on.
		args := []string{
			"-db-url", dbURL, "-base-url", h.baseURL,
			"-scratch-ok", dbName,
			"-conversation", roomName, "-ahead", "10",
			"-cred-file", credFile,
		}
		var code int
		stdout, stderr := captureStdIO(t, func() { code = runRestoreProbeCmd(args) })
		if code != exitPass {
			t.Fatalf("runRestoreProbeCmd = %d, want %d (exitPass); stdout=%s stderr=%s", code, exitPass, stdout, stderr)
		}
		if !strings.Contains(stdout, "ordinals match:        true") {
			t.Errorf("stdout does not report matching ordinals: %q", stdout)
		}
		if stderr == "" {
			t.Fatal("stderr is empty with -quiet off — the logger path was not actually exercised, so the checks below would not catch a leak through it")
		}

		info, err := os.Stat(credFile)
		if err != nil {
			t.Fatalf("stat cred file: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("cred file mode = %v, want 0600", info.Mode().Perm())
		}
		cred, err := readCredentials(credFile)
		if err != nil {
			t.Fatal(err)
		}
		if cred.AccessToken == "" || cred.RefreshToken == "" {
			t.Fatalf("incomplete credentials written: %+v", cred)
		}
		for name, secret := range map[string]string{"access token": cred.AccessToken, "refresh token": cred.RefreshToken} {
			if strings.Contains(stdout, secret) {
				t.Errorf("the %s appears on stdout", name)
			}
			if strings.Contains(stderr, secret) {
				t.Errorf("the %s appears on stderr", name)
			}
		}
	})
}
