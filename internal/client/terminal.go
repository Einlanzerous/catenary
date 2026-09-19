package client

// CANT-123: when a client must stop.
//
// These are CANT-31's record §4, §5 and §6 in the reference client. A client
// that reconnects for ever against a door that will never let it in is the
// failure CANT-31 names — "a revoked device reconnecting forever" — and a
// client that stops when it should have come back is a logout caused by the
// weather. The table below is the record's, implemented twice more in
// TypeScript (CANT-35) and Dart (CANT-42); where this file and the record
// disagree, fix the record first and then all three.

import (
	"fmt"

	"github.com/coder/websocket"

	"github.com/magos/catenary/internal/wire"
)

// TerminalKind is which terminal state a client is in. The two end
// differently and tell the person different things (record §6).
type TerminalKind int

const (
	// NotTerminal is a client that is running, or stopped for any other
	// reason — Kill, Close, a cancelled context.
	NotTerminal TerminalKind = iota
	// TerminalCredential: the credential is gone. Catenary itself refused the
	// stored refresh token. It is re-checked ONCE on relaunch and otherwise
	// ends only on re-enrollment; the person is told to re-enroll this device.
	TerminalCredential
	// TerminalProtocol: close 4001, or a 1008 the table makes permanent. It
	// ends on relaunch — an app update is a relaunch — and the person is told
	// to update the app, or report a bug.
	TerminalProtocol
)

func (k TerminalKind) String() string {
	switch k {
	case TerminalCredential:
		return "credential"
	case TerminalProtocol:
		return "protocol"
	default:
		return "none"
	}
}

// Terminal is the state Status reports: which terminal, and the observation
// that put the client there. The zero value is "not terminal".
type Terminal struct {
	Kind   TerminalKind
	Reason string
}

// TerminalError is what Run returns when it stopped because it must not
// reconnect. NEITHER TERMINAL ENDS ON A TIMER OR A NETWORK CHANGE: Run has
// returned, and nothing in this package will dial again. A relaunch — a new
// Client over the same Journal — is what ends a protocol terminal and what
// re-checks a credential one, exactly once, by simply running.
//
// NOTHING IS DELETED. The Journal still holds the credential and the whole
// local store, so a terminal that turns out to be false is recovered by a
// relaunch, and a true one loses nothing: the server refuses that credential
// for ever anyway.
type TerminalError struct{ Terminal }

func (e *TerminalError) Error() string {
	return fmt.Sprintf("client: terminal (%s): %s", e.Kind, e.Reason)
}

// closeVerdict is what record §4 says to do about one ended session.
type closeVerdict int

const (
	reconnect           closeVerdict = iota // with backoff, then catch up
	reconnectAtMaximum                      // error{internal}: never terminal, never eager
	stopProtocolFailure                     // terminal
)

// statusRevoked and statusPolicyViolation are the two close codes with a
// terminal reading. 4001 is hub.StatusRevoked, spelled as a number because a
// client does not import the server's hub; the record is what pins it.
const (
	statusRevoked         = websocket.StatusCode(4001)
	statusPolicyViolation = websocket.StatusPolicyViolation
)

// classifyClose is record §4's table, and only that.
//
// preceding is the LAST frame before the close, and only if it was an `error`
// carrying no client_id — nil otherwise. An `error` that names a send is that
// send's answer, not the session's: an earlier error{rate_limited} about some
// message does not make a later bare 1008 read as transient.
//
// A BARE 1008 IS A CLIENT BUG. Five sites produce one — a first frame that is
// not a hello, a second hello, a malformed frame, and two the generated
// decoder makes unreachable — and the sixth, the hello timeout, has closed
// 4002 since CANT-122 precisely so that this line could be written. Replaying
// the same bytes walks into the same close.
//
// EVERYTHING NOT LISTED RECONNECTS, including a code a later server adds. The
// default is the safe direction: a client that reconnects when it should have
// stopped wastes a dial, and one that stops when it should have reconnected
// has logged its person out.
func classifyClose(status websocket.StatusCode, preceding *wire.ServerError) (closeVerdict, string) {
	switch status {
	case statusRevoked:
		return stopProtocolFailure, "close 4001: the credential behind the session was revoked"
	case statusPolicyViolation:
		if preceding == nil {
			return stopProtocolFailure, "close 1008, bare: the server could not accept what this client wrote"
		}
		switch preceding.Code {
		case wire.ErrorCodeUnauthorized, wire.ErrorCodeWireVersionUnsupported:
			return stopProtocolFailure, fmt.Sprintf("close 1008 after error{%s}", preceding.Code)
		case wire.ErrorCodeInternal:
			// `retryable` IS NOT CONSULTED. An unclassified server failure
			// arrives as `internal` with that flag biased toward true, and a
			// clearly permanent server fault must not stop every client at
			// once and keep them stopped after the fix.
			return reconnectAtMaximum, ""
		}
	}
	return reconnect, ""
}
