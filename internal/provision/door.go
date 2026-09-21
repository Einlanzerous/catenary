package provision

// The door — CANT-131 criterion 9, ruling 2's static service credential.
//
// ONE CREDENTIAL, PRESENTED AS `Authorization: Bearer`, COMPARED CONSTANT-TIME
// OVER DIGESTS. Nothing in the database can open this surface, which is the
// property ruling 2 was picked for: no SQL injection, no restored backup and no
// stolen device token can mint the one credential that could enroll a device as
// anybody. The corollary is stated rather than implied — a DEVICE access token, a
// REFRESH token and a BOT token are simply WRONG CREDENTIALS here. They are not
// "insufficient", there is no scope check, and there is nothing this surface
// could do with them: it never calls store.Authenticate at all.
//
// THE SAME ANSWER FOR WRONG AND FOR ABSENT, IN THE SAME CODE PATH. A missing
// header, a header that is not `Bearer …`, and a wrong token all take the
// identical route through opens() — the presented string is hashed and compared
// whatever it is, including when it is empty — and all three write the identical
// 401 body. enrollHandler's own comment carries the argument for one refusal
// shape on a credential route with no rate limiter in front of it, and it applies
// here with one addition: a surface that answered differently for "no header"
// than for "wrong token" would tell a prober, in one request, whether it had
// found a service that even has a credential to guess.
//
// THE REFUSAL LOG CARRIES NOTHING THAT WAS PRESENTED. Not the header, not a
// prefix of it, not its length. `reason` distinguishes absent from malformed from
// mismatch, and that is the direction information is allowed to travel: the
// service log may know more than the response body does (the plan's own rule for
// declining a rate limiter here), and none of the three values says anything
// about the bytes. The METHOD and the PATH are logged, and no credential ever
// rides in either — this door reads one header and nothing else, for the reason
// cmd/catenary's bearer() gives for never reading a query parameter.

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/magos/catenary/internal/store"
)

// equalDigests is the constant-time comparison.
//
// EQUAL LENGTHS BY CONSTRUCTION, WHICH IS WHY THE HASH IS THERE AT ALL.
// subtle.ConstantTimeCompare returns 0 immediately when two slices differ in
// length, so comparing the presented credential to the configured one directly
// would leak its length through timing — and the length of a secret is a real
// part of it. Both sides here are SHA-256 digests, so both are exactly 32 bytes
// whatever was presented, and the comparison takes the same time for an empty
// header as for a token that is wrong in its last byte.
func equalDigests(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// doorFault breaks the credential comparison on purpose, for criterion 9's
// negative controls — store.personGuardFault's own shape and for the same
// reason. A comparison is the one thing in this package that can be wrong while
// every positive test still passes: a door that opens for everything passes every
// "the right token works" test there is, and a door that compares a prefix
// passes every test whose wrong token differs in its first byte. So each way of
// getting it wrong is a value here, and a test is watched failing with it set.
type doorFault int

const (
	// doorFaultNone is the shipped path: the whole digest, constant-time.
	doorFaultNone doorFault = iota

	// doorFaultPrefixCompare is `==` on the first prefixCompareBytes bytes of
	// the plaintext — a plausible-looking hand-rolled comparison that is both
	// variable-time and wrong. It opens for any credential that shares a prefix
	// with the real one, which is precisely what an attacker who has seen a
	// truncated token in a log would present.
	doorFaultPrefixCompare

	// doorFaultAlwaysOpen removes the check entirely. It exists so that the
	// tests which assert 401 can be shown to be asserting something: with this
	// set, a request carrying no credential at all reaches a handler.
	doorFaultAlwaysOpen
)

// prefixCompareBytes is how much of the credential doorFaultPrefixCompare
// looks at. Eight, because it has to be short enough that a test can present a
// token sharing it and long enough that it is not obviously absurd — the fault
// is meant to look like something somebody would write.
const prefixCompareBytes = 8

// authorize is the door. It reports whether the request may proceed, and has
// already written the refusal when it may not.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	token, shape := presented(r)

	// THE COMPARISON RUNS FOR EVERY REQUEST, INCLUDING THE ONES WITH NO
	// CREDENTIAL, and that is what makes "the same timing class" true rather
	// than claimed. `shape` is computed above and read only below, after the
	// comparison has already happened, so no branch on it can shorten the path
	// that a wrong token takes.
	if s.opens(token) {
		return true
	}

	reason := shape
	if reason == shapeBearer {
		reason = "mismatch"
	}
	s.d.Logger.WarnContext(r.Context(), "provisioning credential refused",
		"reason", reason, "method", r.Method, "path", r.URL.Path)
	writeError(w, http.StatusUnauthorized, errUnauthorized)
	return false
}

// opens is the comparison itself.
//
// The zero fault is the shipped path and it is the first thing a reader should
// see; the two broken ones are below it and are unreachable outside this
// package's tests.
func (s *Server) opens(presented string) bool {
	switch s.fault {
	case doorFaultPrefixCompare:
		if len(presented) < prefixCompareBytes || len(s.d.Token) < prefixCompareBytes {
			return false
		}
		return presented[:prefixCompareBytes] == s.d.Token[:prefixCompareBytes]
	case doorFaultAlwaysOpen:
		return true
	}
	// BOTH SIDES ARE 32-BYTE DIGESTS, so the compare is over equal lengths and
	// leaks neither the length nor the position of the first differing byte.
	// s.want was computed once at New; nothing here hashes the configured
	// secret again.
	return equalDigests(store.HashToken(presented), s.want)
}

// The three shapes an Authorization header can arrive in. Values, not booleans,
// because they reach the log as one attribute and an operator reads them.
const (
	shapeAbsent    = "absent"
	shapeMalformed = "malformed"
	shapeBearer    = "bearer"
)

// presented pulls the credential off the request, and says how the header was
// shaped so the refusal log can distinguish "nothing was sent" from "something
// unparseable was sent" WITHOUT the response distinguishing them.
//
// A LOCAL FIVE LINES RATHER THAN A SHARED HELPER, deliberately. cmd/catenary's
// bearer() serves the member-facing transport, where the token is a device's
// access token and every change to it is a change to how a client authenticates.
// This door refuses on entirely different terms — one static secret, no store
// lookup, no session — and sharing the extraction would mean a change made for
// the wire surface silently changed what opens the provisioning surface. The
// duplication is four lines; the coupling would be a credential.
func presented(r *http.Request) (token, shape string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", shapeAbsent
	}
	const prefix = "Bearer "
	// EqualFold on the scheme, as cmd/catenary's bearer does: RFC 7235 makes the
	// scheme case-insensitive, and a connector sending `bearer` is presenting a
	// credential rather than making a mistake about one.
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", shapeMalformed
	}
	return h[len(prefix):], shapeBearer
}
