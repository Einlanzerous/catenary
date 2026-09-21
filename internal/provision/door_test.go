package provision

// CANT-131 criterion 9 — the door fails closed.
//
// The boot half of that criterion (neither variable, a half-configured pair, a
// token under the floor) is internal/config's and cmd/catenary's; the real
// credentials that must not open this door — a device access token, a refresh
// token, a bot token — need a Postgres and live in cmd/catenary/provision_test.go.
// WHAT IS HERE IS THE COMPARISON ITSELF, and its negative controls, because a
// credential check is the one thing in this package that can be wrong while every
// positive test still passes.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

// The credential these tests configure: 32 bytes, config.MinProvisionTokenBytes,
// and a LITERAL rather than a random value so that the assertion which searches
// the refusal log for every prefix of it is the same every run.
//
// IT READS AS A SENTENCE RATHER THAN AS A CREDENTIAL, ON ci.yml's OWN ARGUMENT —
// the first version was 32 random base64url characters, which is what Signet
// generates, and GitGuardian reported it as a Generic High Entropy Secret on the
// pull request. ci.yml deleted a password-shaped Postgres literal for exactly that
// reason. What these tests need is the length, the mixed case and a distinctive
// prefix; entropy buys none of it.
//
// TWO PROPERTIES ARE LOAD BEARING. The capital T, so that the lower-cased
// credential in TestOpensComparesTheWholeCredential is a DIFFERENT string and the
// case column means something. And the prefix: no four bytes of this value appear
// in the refusal log line, which is what lets that assertion start at four rather
// than at the whole string.
const testToken = "not-a-secret-just-a-Test-token-1"

// recorder keeps every log record so a test can assert on levels and attributes
// — cmd/catenary's own test recorder, which is in package main and cannot be
// reached from here.
type recorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

// lines renders every record as one searchable string — message and every
// attribute — which is what an assertion about "the line carries nothing that was
// presented" has to look at.
func (r *recorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.recs {
		var b strings.Builder
		b.WriteString(rec.Level.String())
		b.WriteString(" ")
		b.WriteString(rec.Message)
		rec.Attrs(func(a slog.Attr) bool {
			b.WriteString(" ")
			b.WriteString(a.Key)
			b.WriteString("=")
			b.WriteString(a.Value.String())
			return true
		})
		out = append(out, b.String())
	}
	return out
}

// reached counts calls that got past the door.
type reached struct{ n int }

func testServer(t *testing.T, log slog.Handler, hit *reached) *Server {
	t.Helper()
	return New(Deps{
		Logger: slog.New(log),
		Token:  testToken,
		Ensure: func(context.Context, string, string) (store.EnsuredPerson, error) {
			hit.n++
			return store.EnsuredPerson{
				Account: store.PersonAccount{UserID: uuid.New(), Email: "ada@example.com",
					Handle: "ada", DisplayName: "Ada", Status: store.PersonStatusActive},
				Token:   store.IssuedToken{Plaintext: strings.Repeat("a", 43)},
				Outcome: store.PersonCreated,
			}, nil
		},
		Lookup: func(context.Context, string) (store.PersonLookup, error) {
			hit.n++
			return store.PersonLookup{}, store.ErrPersonNotFound
		},
		Deactivate: func(context.Context, uuid.UUID) (store.Offboard, error) {
			hit.n++
			return store.Offboard{}, store.ErrPersonNotFound
		},
	})
}

func ensurePost() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/accounts",
		strings.NewReader(`{"email":"ada@example.com","display_name":"Ada"}`))
}

// TestTheCredentialOpensTheDoorAndNothingElseDoes is the positive half and the
// negative half in one table, because the thing being asserted is that they
// differ in exactly one way.
func TestTheCredentialOpensTheDoorAndNothingElseDoes(t *testing.T) {
	// WHAT IS PRESENTED, AND WHY EACH ONE IS IN THE TABLE.
	cases := []struct {
		name   string
		header string // "" means no Authorization header at all
		open   bool
	}{
		{"the credential", "Bearer " + testToken, true},
		{"the credential, scheme in lower case", "bearer " + testToken, true},
		{"no header at all", "", false},
		{"the scheme with nothing after it", "Bearer ", false},
		{"the scheme alone", "Bearer", false},
		{"another scheme entirely", "Basic " + testToken, false},
		{"the credential with no scheme", testToken, false},
		// A CREDENTIAL THAT SHARES A PREFIX. This is what an attacker who saw a
		// truncated token in somebody's log presents, and it is the case a
		// hand-rolled prefix comparison accepts — see the control below.
		{"the first eight bytes and then something else", "Bearer " + testToken[:8] + "wrongwrongwrongwrongwrongwrong", false},
		{"one byte short", "Bearer " + testToken[:len(testToken)-1], false},
		{"one byte too many", "Bearer " + testToken + "x", false},
		{"one byte different, at the end", "Bearer " + testToken[:len(testToken)-1] + "Q", false},
		{"a token of the right length that is simply not ours", "Bearer " + strings.Repeat("A", len(testToken)), false},
		{"the empty string, explicitly", "Bearer \"\"", false},
	}

	var refusals [][]byte
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit := &reached{}
			srv := testServer(t, &recorder{}, hit)
			req := ensurePost()
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if tc.open {
				if rec.Code != http.StatusCreated {
					t.Fatalf("the credential was refused: %d %s", rec.Code, rec.Body.String())
				}
				if hit.n != 1 {
					t.Fatalf("the call did not reach the store (%d calls)", hit.n)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("= %d, want 401: %s", rec.Code, rec.Body.String())
			}
			// THE STORE IS NEVER REACHED. A door that refused after the write
			// would be a door that had already created the account.
			if hit.n != 0 {
				t.Errorf("a refused call reached the store %d time(s)", hit.n)
			}
			refusals = append(refusals, rec.Body.Bytes())
		})
	}

	// BYTE-IDENTICAL, ALL OF THEM. "Wrong" and "absent" and "malformed" are one
	// answer: any difference — a field, a word, an ordering — tells a prober in
	// one request whether this service even has a credential to guess, and
	// whether what it just sent was close.
	//
	// Guarded rather than indexed, because the interesting run is the one where a
	// broken comparison let every case through: the sub-tests above have already
	// reported that, and this must add a sentence rather than a panic on an empty
	// slice. (It did panic, once, on exactly that run.)
	if len(refusals) == 0 {
		t.Fatal("not one case was refused, so there is nothing to compare — the comparison is open")
	}
	for i := 1; i < len(refusals); i++ {
		if string(refusals[i]) != string(refusals[0]) {
			t.Fatalf("two refusals differ:\n  %q\n  %q", refusals[0], refusals[i])
		}
	}
	if want := "{\"error\":\"unauthorized\"}\n"; string(refusals[0]) != want {
		t.Errorf("the refusal body is %q, want %q", refusals[0], want)
	}
}

// TestOpensComparesTheWholeCredential drives the comparison directly, including
// the empty string, which is the input the door hands it when no header was sent
// at all.
//
// THE TIMING CLAIM IS STRUCTURAL AND THIS IS WHERE IT IS ARGUED. A wall-clock
// test of a constant-time comparison is a flaky test, so what is asserted
// mechanically is the shape the claim rests on: ONE function decides, it is
// reached for every input including the empty one, and both sides of the compare
// are 32-byte digests so neither the length of the presented credential nor the
// position of its first wrong byte changes the work done.
// TestThereIsExactlyOneRefusalPathInThisPackage below is the other half — that
// nothing else in the package can write a 401 by a different route.
func TestOpensComparesTheWholeCredential(t *testing.T) {
	srv := testServer(t, &recorder{}, &reached{})
	for _, tc := range []struct {
		presented string
		open      bool
	}{
		{testToken, true},
		{"", false},
		{testToken[:8] + "wrongwrongwrong", false},
		{testToken[:len(testToken)-1], false},
		{testToken + "x", false},
		{strings.ToLower(testToken), false},
	} {
		if got := srv.opens(tc.presented); got != tc.open {
			t.Errorf("opens(%d bytes) = %v, want %v", len(tc.presented), got, tc.open)
		}
	}
}

// TestTheRefusalLogCarriesNothingThatWasPresented is criterion 9's log clause.
//
// The WARN line is the one place a wrong credential leaves a trace, and a line
// carrying a prefix of it turns the service log into the oracle the response
// refuses to be — logs are shipped to Dozzle and Datadog, read by more people
// than can read the database, and kept.
func TestTheRefusalLogCarriesNothingThatWasPresented(t *testing.T) {
	// A wrong credential, long enough that its prefixes are distinctive, plus the
	// REAL one presented under the wrong scheme — so the assertion covers both a
	// credential that is wrong and a credential that is right but unusable.
	for _, presented := range []string{
		"hunter2hunter2hunter2hunter2hunter2hunter2X",
		testToken,
	} {
		log := &recorder{}
		srv := testServer(t, log, &reached{})
		req := ensurePost()
		req.Header.Set("Authorization", "Basic "+presented)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("= %d, want 401", rec.Code)
		}

		lines := log.lines()
		if len(lines) != 1 {
			t.Fatalf("a refusal wrote %d log lines, want exactly one: %v", len(lines), lines)
		}
		line := lines[0]
		if !strings.HasPrefix(line, slog.LevelWarn.String()) {
			t.Errorf("the refusal is not at WARN: %q", line)
		}

		// EVERY PREFIX, FROM FOUR BYTES UP, and the whole thing. Four rather than
		// one because a single character of a base64url alphabet appears in
		// ordinary English words and in the word "unauthorized"; four characters
		// of a credential appearing in a log line is a leak by any measure.
		for n := 4; n <= len(presented); n++ {
			if strings.Contains(line, presented[:n]) {
				t.Errorf("the refusal line carries the first %d bytes of what was presented: %q", n, line)
				break
			}
		}
		// AND NOT ITS LENGTH. A line that said "39 bytes" would narrow the guess
		// for anybody who could make one request.
		if strings.Contains(line, strconv.Itoa(len(presented))) {
			t.Errorf("the refusal line carries the length of what was presented: %q", line)
		}
	}
}

// TestTheRefusalLogTellsTheThreeShapesApart is the other direction, and it is
// deliberate rather than accidental: the RESPONSE distinguishes nothing, and the
// LOG distinguishes absent from malformed from wrong. Visibility standing in for
// a rate limiter is the plan's own argument, and it is only worth anything if the
// information travels one way.
func TestTheRefusalLogTellsTheThreeShapesApart(t *testing.T) {
	for _, tc := range []struct{ header, reason string }{
		{"", "absent"},
		{"Basic abc", "malformed"},
		{"Bearer " + strings.Repeat("A", len(testToken)), "mismatch"},
	} {
		log := &recorder{}
		srv := testServer(t, log, &reached{})
		req := ensurePost()
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		srv.ServeHTTP(httptest.NewRecorder(), req)

		lines := log.lines()
		if len(lines) != 1 || !strings.Contains(lines[0], "reason="+tc.reason) {
			t.Errorf("a %q header logged %v, want one line with reason=%s", tc.header, lines, tc.reason)
		}
	}
}

// ---------------------------------------------------------------------------
// The negative controls — the comparison is watched being wrong
//
// Each of these asserts the BROKEN behaviour, which is what makes the positive
// test above load-bearing: with the fault set, the case the table asserts is
// refused is accepted instead, so the table is measuring the comparison and not
// merely agreeing with it. store.personGuardFault's own pattern.

// TestWithoutTheWholeComparisonASharedPrefixOpensTheDoor is what
// "the first eight bytes and then something else" in the table above is for.
func TestWithoutTheWholeComparisonASharedPrefixOpensTheDoor(t *testing.T) {
	hit := &reached{}
	srv := testServer(t, &recorder{}, hit)
	srv.fault = doorFaultPrefixCompare

	req := ensurePost()
	req.Header.Set("Authorization", "Bearer "+testToken[:8]+"wrongwrongwrongwrongwrongwrong")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || hit.n != 1 {
		t.Fatalf("the prefix-comparison fault did not open the door (%d, %d store calls) — "+
			"the control proves nothing, so the table above may be passing for the wrong reason",
			rec.Code, hit.n)
	}
	t.Log("with `==` on the first eight bytes, a credential sharing a prefix creates an account")
}

// TestWithoutAnyComparisonEveryRequestOpensTheDoor is what every 401 case in the
// table is for. With the check removed, a request carrying NO credential at all
// reaches the store.
func TestWithoutAnyComparisonEveryRequestOpensTheDoor(t *testing.T) {
	hit := &reached{}
	srv := testServer(t, &recorder{}, hit)
	srv.fault = doorFaultAlwaysOpen

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, ensurePost())

	if rec.Code != http.StatusCreated || hit.n != 1 {
		t.Fatalf("the always-open fault did not open the door (%d, %d store calls)", rec.Code, hit.n)
	}
	t.Log("with no comparison, a request with no Authorization header at all creates an account")
}

// ---------------------------------------------------------------------------
// The structural halves

// TestThereIsExactlyOneRefusalPathInThisPackage is the mechanical form of
// criterion 9's "the same code path for wrong and absent".
//
// A SOURCE SCAN, in the idiom of internal/store's own guards, because there is
// nothing to type-check: a second `writeError(w, http.StatusUnauthorized, …)`
// added later — a scope check, a different message for an expired credential, a
// distinct answer for a malformed header — would be a second refusal path, and
// two refusal paths are two places for them to stop being identical. One
// occurrence, in door.go, or this fails and names what it found.
func TestThereIsExactlyOneRefusalPathInThisPackage(t *testing.T) {
	var found []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Comments are exempt: this test's own subject has to be nameable in
			// prose, and the line above proves the point.
			code, _, _ := strings.Cut(line, "//")
			if strings.Contains(code, "StatusUnauthorized") {
				found = append(found, name+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(found) != 1 {
		t.Errorf("%d place(s) in this package write a 401, want exactly one: %v\n\n"+
			"A missing credential, a malformed header and a wrong token are ONE refusal with one "+
			"body and one code path. A second place that writes a 401 is a second body waiting to "+
			"drift from the first, and the difference is measurable by anybody who can make two "+
			"requests.", len(found), found)
	}
}

// TestNewRefusesAHalfWiredDoor. The empty token is the case that matters: a
// Server built with one would hold the digest of the empty string and open for a
// request carrying no credential at all, which is the one wiring mistake here
// that cannot be seen from the outside.
func TestNewRefusesAHalfWiredDoor(t *testing.T) {
	ok := Deps{
		Logger:     slog.New(slog.DiscardHandler),
		Token:      testToken,
		Ensure:     func(context.Context, string, string) (store.EnsuredPerson, error) { return store.EnsuredPerson{}, nil },
		Lookup:     func(context.Context, string) (store.PersonLookup, error) { return store.PersonLookup{}, nil },
		Deactivate: func(context.Context, uuid.UUID) (store.Offboard, error) { return store.Offboard{}, nil },
	}
	for _, tc := range []struct {
		name  string
		wreck func(*Deps)
	}{
		{"no logger", func(d *Deps) { d.Logger = nil }},
		{"no token", func(d *Deps) { d.Token = "" }},
		{"no ensure", func(d *Deps) { d.Ensure = nil }},
		{"no lookup", func(d *Deps) { d.Lookup = nil }},
		{"no deactivate", func(d *Deps) { d.Deactivate = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ok
			tc.wreck(&d)
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("New built a half-wired door instead of panicking at composition")
				}
			}()
			_ = New(d)
		})
	}

	// And the complete one builds.
	if New(ok) == nil {
		t.Fatal("New returned nil for a complete Deps")
	}
}

// TestAnEmptyTokenWouldOpenForAnEmptyCredential is why the panic above is a
// panic. It builds the forbidden Server by hand — around New, which refuses it —
// and shows what the door would then do, so the guard is measured rather than
// asserted.
func TestAnEmptyTokenWouldOpenForAnEmptyCredential(t *testing.T) {
	forbidden := &Server{d: Deps{Token: ""}, want: store.HashToken("")}
	if !forbidden.opens("") {
		t.Fatal("this control is stale: an empty configured token no longer matches an empty " +
			"presented credential, so New's panic is guarding something else now")
	}
}
