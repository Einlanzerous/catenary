package main

// CANT-109 — `soakrig provision` against a real local server: creates
// exactly one account and device through the real store function and the
// real POST /enroll path, refuses an already-taken handle rather than
// reusing the account, and writes the credential file at 0600 with nothing
// secret ever reaching stdout, stderr or the logger.
//
// Gated on CATENARY_TEST_DATABASE_URL, the same convention broken_test.go
// uses: unset skips rather than fails.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magos/catenary/internal/store"
)

// testCatenaryBin builds the catenary binary ONCE per test binary run and
// hands every caller the same path — resolveBinary itself would otherwise
// re-run `go build` for every test that starts a local server, which is the
// bulk of this file's own runtime.
var (
	testBinOnce sync.Once
	testBinPath string
	testBinErr  error
)

func testCatenaryBin(t *testing.T) string {
	t.Helper()
	testBinOnce.Do(func() {
		testBinPath, testBinErr = resolveBinary(Config{})
	})
	if testBinErr != nil {
		t.Fatalf("build catenary for the test: %v", testBinErr)
	}
	return testBinPath
}

// startLocalServer runs a real `catenary serve` subprocess against dbURL —
// the SAME machinery `soak` itself uses (freePort, harness.startServer) —
// and tears it down at test end. Every CANT-109 test in this package runs
// against a REAL local server, per the ticket's own Done-when.
func startLocalServer(t *testing.T, dbURL string) *harness {
	t.Helper()
	h := &harness{cfg: Config{DBURL: dbURL}, hello: newHelloHistogram()}
	h.bin = testCatenaryBin(t)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	h.port = port
	h.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	pool, err := store.ConnectWithRetry(context.Background(), dbURL, 30*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	h.pool = pool
	h.store = store.New(pool, store.DefaultLimits(), h.cfg.logger())
	if err := h.startServer(context.Background()); err != nil {
		t.Fatalf("start the local server: %v", err)
	}
	t.Cleanup(h.cleanup)
	return h
}

// captureStdIO redirects os.Stdout and os.Stderr for the duration of fn and
// returns what each collected — the "capture stdout, stderr and the logger
// output" the ticket asks for, at the level a real operator actually sees.
func captureStdIO(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW

	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = outBuf.ReadFrom(outR) }()
	go func() { defer wg.Done(); _, _ = errBuf.ReadFrom(errR) }()

	fn()

	os.Stdout, os.Stderr = origOut, origErr
	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	return outBuf.String(), errBuf.String()
}

func TestProvisionAgainstALocalServer(t *testing.T) {
	dbURL := soakDBFixture(t)
	h := startLocalServer(t, dbURL)

	t.Run("creates exactly one account and device", func(t *testing.T) {
		credFile := filepath.Join(t.TempDir(), "creds.json")
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))

		cred, err := runProvision(context.Background(), dbURL, h.baseURL, "provisioned-person", "Provisioned Person", credFile, nil, logger)
		if err != nil {
			t.Fatalf("runProvision: %v", err)
		}
		if cred.UserID == "" || cred.DeviceID == "" || cred.AccessToken == "" || cred.RefreshToken == "" {
			t.Fatalf("incomplete credentials: %+v", cred)
		}

		var userCount int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM users WHERE handle = $1`, "provisioned-person").Scan(&userCount); err != nil {
			t.Fatal(err)
		}
		if userCount != 1 {
			t.Errorf("users with that handle = %d, want exactly 1", userCount)
		}
		var deviceCount int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM devices WHERE user_id = (SELECT id FROM users WHERE handle = $1)`,
			"provisioned-person").Scan(&deviceCount); err != nil {
			t.Fatal(err)
		}
		if deviceCount != 1 {
			t.Errorf("devices for that user = %d, want exactly 1", deviceCount)
		}

		info, err := os.Stat(credFile)
		if err != nil {
			t.Fatalf("stat cred file: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("cred file mode = %v, want 0600", info.Mode().Perm())
		}
		readBack, err := readCredentials(credFile)
		if err != nil {
			t.Fatalf("readCredentials: %v", err)
		}
		if readBack.AccessToken != cred.AccessToken || readBack.DeviceID != cred.DeviceID {
			t.Errorf("read-back credentials do not match what was written: %+v vs %+v", readBack, cred)
		}

		logged := logBuf.String()
		if strings.Contains(logged, cred.AccessToken) || strings.Contains(logged, cred.RefreshToken) {
			t.Errorf("the access or refresh token appears in the logger's own output: %s", logged)
		}
	})

	t.Run("refuses an existing handle", func(t *testing.T) {
		dir := t.TempDir()
		logger := slog.New(slog.DiscardHandler)

		if _, err := runProvision(context.Background(), dbURL, h.baseURL, "duplicate-handle", "First", filepath.Join(dir, "a.json"), nil, logger); err != nil {
			t.Fatalf("first provision: %v", err)
		}

		_, err := runProvision(context.Background(), dbURL, h.baseURL, "duplicate-handle", "Second", filepath.Join(dir, "b.json"), nil, logger)
		if err == nil {
			t.Fatal("a second provision with the same handle succeeded; want a refusal")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("error does not say the handle already exists: %v", err)
		}

		var userCount int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM users WHERE handle = $1`, "duplicate-handle").Scan(&userCount); err != nil {
			t.Fatal(err)
		}
		if userCount != 1 {
			t.Errorf("users with that handle = %d, want exactly 1 (the refused attempt must not have created a second)", userCount)
		}
		if _, err := os.Stat(filepath.Join(dir, "b.json")); err == nil {
			t.Errorf("a credential file was written for the refused second attempt")
		}
	})

	t.Run("the CLI never prints a secret, and writes nothing to stdout", func(t *testing.T) {
		credFile := filepath.Join(t.TempDir(), "creds.json")
		const cfID, cfSecret = "unit-test-cf-id", "unit-test-cf-secret-xyz"
		args := []string{
			"-db-url", dbURL, "-base-url", h.baseURL,
			"-handle", "cli-provisioned", "-display-name", "CLI Provisioned",
			"-cred-file", credFile,
			"-cf-access-client-id", cfID, "-cf-access-client-secret", cfSecret,
		}
		var code int
		stdout, stderr := captureStdIO(t, func() { code = runProvisionCmd(args) })
		if code != exitPass {
			t.Fatalf("runProvisionCmd = %d, want %d; stderr=%s", code, exitPass, stderr)
		}
		if stdout != "" {
			t.Errorf("provision wrote to stdout: %q — nothing may reach stdout", stdout)
		}
		cred, err := readCredentials(credFile)
		if err != nil {
			t.Fatal(err)
		}
		for name, secret := range map[string]string{
			"access token": cred.AccessToken, "refresh token": cred.RefreshToken, "cf-access-client-secret": cfSecret,
		} {
			if strings.Contains(stdout, secret) {
				t.Errorf("the %s appears on stdout", name)
			}
			if strings.Contains(stderr, secret) {
				t.Errorf("the %s appears on stderr: %q", name, stderr)
			}
		}
	})
}
