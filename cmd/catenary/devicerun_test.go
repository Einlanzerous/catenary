package main

// CANT-210 — a server a phone can reach. The Go rigs in this package start a
// server inside `go test` for a client on the same machine, on a loopback port
// httptest picks; nothing mints an enrollment token from the command line and
// there is no seed command. A device run needs all three, so this is the rig
// for it: the canvas's corpus, enough further messages that the bootstrap is a
// real one, a listener on an address the LAN can reach, and two invitations.
//
// IT SERVES UNTIL INTERRUPTED, so it is a manual lane and not a check: CI and
// verify.sh leave CATENARY_DEVICE_RUN unset and it skips. Run it with
//
//	CATENARY_TEST_DATABASE_URL=postgres://... \
//	CATENARY_DEVICE_RUN=192.168.1.10:4099 \
//	  go test ./cmd/catenary -run '^TestServeForADevice$' -count=1 -timeout 0 -v
//
// `-timeout 0` because go test's own ten-minute limit would end the run, and
// `-v` because without it go test holds the output, the tokens included, until
// the test ends. Ctrl-C stops it and drains the way `catenary serve` does.
//
// Gated like websmoke_test.go: unset skips, and set with no database FAILS.
// processFixture skips without CATENARY_TEST_DATABASE_URL, which here would be
// a green line and no server for the person holding the phone.
//
// TWO PEOPLE, NOT ONE. A person has one redeemable enrollment token at a time
// (IssueEnrollmentToken supersedes the earlier ones), so the phone and its peer
// enroll as different people: the phone as hollis, the reader the canvas is
// drawn for, and the browser as nadia. Nadia by name and not "someone who
// shares a group with hollis": seedCanvas ends by deactivating petra and
// oskar, and petra shares a group with hollis.
//
// The peer is the web client under `vite dev`, because this binary serves the
// API and no page: the test prints the command.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/magos/catenary/internal/store"
)

// deviceRunMessages is how many messages hollis must be able to see before
// the server opens: CANT-200's frame measurement is of a bootstrap of at
// least this many.
const deviceRunMessages = 500

func deviceRunAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("CATENARY_DEVICE_RUN")
	if addr == "" {
		t.Skip("CATENARY_DEVICE_RUN not set; skipping the device run's server (set it to a listen address the phone can reach, such as 192.168.1.10:4099)")
	}
	if os.Getenv("CATENARY_TEST_DATABASE_URL") == "" {
		t.Fatal("CATENARY_DEVICE_RUN is set and CATENARY_TEST_DATABASE_URL is not: the device run serves a seeded database, and a skip here would be a server nobody started")
	}
	return addr
}

func TestServeForADevice(t *testing.T) {
	addr := deviceRunAddr(t)

	// The fixture's own context ends after two minutes, which is right for a
	// seed and wrong for a server, so only the seed runs on it.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, pool, st, d := processFixture(t, logger)
	r := &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, since: dbNow(ctx, t, pool)}
	c := seedCanvas(t, r)

	// Into Shed Projects, which hollis is in and has muted: the bootstrap is
	// as large as the criterion asks, and Kitchen Table is still the canvas's.
	// Then Kitchen Table is touched last, so the rail still opens on it.
	seen := visibleMessages(ctx, t, st, c)
	for i := seen; i < deviceRunMessages; i++ {
		send(ctx, t, st, c.shed, c.marek, fmt.Sprintf("shed log %03d — tension checked, nothing to report", i+1))
	}
	send(ctx, t, st, c.kitchen, c.nadia, "I’ll be on the browser if anyone needs me.")
	if seen = visibleMessages(ctx, t, st, c); seen < deviceRunMessages {
		t.Fatalf("hollis can see %d messages, want at least %d", seen, deviceRunMessages)
	}

	hollis, err := st.IssueEnrollmentToken(ctx, c.hollis)
	if err != nil {
		t.Fatalf("IssueEnrollmentToken(hollis): %v", err)
	}
	nadia, err := st.IssueEnrollmentToken(ctx, c.nadia)
	if err != nil {
		t.Fatalf("IssueEnrollmentToken(nadia): %v", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("CATENARY_DEVICE_RUN=%s: %v", addr, err)
	}
	base := "http://" + ln.Addr().String()

	// To stderr and not t.Log: this is what the person at the phone reads, and
	// it has to be on the terminal before the test ends.
	fmt.Fprintf(os.Stderr, `
== the device run's server ==
address   %[1]s
messages  hollis can see %[2]d

hollis (the phone)
  token   %[3]s
  link    %[1]s/#enroll=%[3]s

nadia (the browser)
  token   %[4]s
  peer    cd web && CATENARY_DEV_API=%[1]s npx vite
          then http://localhost:4009, and the token in its account view

Each token enrolls one device. Ctrl-C stops the server.

`, base, seen, hollis.Plaintext, nadia.Plaintext)

	run, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(run, d, ln, nil); err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// visibleMessages counts what /sync hands hollis from a null cursor to the
// head: the same pages a freshly enrolled device bootstraps from.
func visibleMessages(ctx context.Context, t *testing.T, st *store.Store, c canvas) int {
	t.Helper()
	n := 0
	for after := int64(0); ; {
		page, err := serveSync(ctx, st, c.hollis, after, 0)
		if err != nil {
			t.Fatalf("sync after %d: %v", after, err)
		}
		n += len(page.Messages)
		if !page.HasMore {
			return n
		}
		after = int64(page.LogSeq)
	}
}
