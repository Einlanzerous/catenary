package main

// CANT-119 — sendWasRefused, tested directly.
//
// THIS FILE EXISTS BECAUSE THE END-TO-END TEST CANNOT DO THIS JOB, and the
// review of PR #67 is what made that clear. TestAllSendsRefusedCountsAsServer-
// Failure drives a real refusal through a real server, which is worth having —
// but it cannot tell a *SendError apart from any other unrecognised error,
// because the first version of sendWasRefused routed both down the same
// fall-through. A negative control that flipped that fall-through therefore
// broke both behaviours at once and could attribute the failure to neither:
// the control fired, and proved nothing about the default direction.
//
// A table over the function itself is what separates them. The case that
// matters most is "a brand new error nobody enumerated" — nothing anywhere
// covered it before, and it is the one that decides whether an unforeseen
// failure is reported as the server's fault or as the harness's.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/magos/catenary/internal/client"
)

func TestSendWasRefusedOnlyCountsTheServerAnswering(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error at all", nil, false},

		// The harness's own conditions. None of these is the server saying
		// anything, so none may push a run towards server_failure.
		{"this rig's per-send deadline", context.DeadlineExceeded, false},
		{"the run's context cancelled", context.Canceled, false},
		{"no socket open", client.ErrNotConnected, false},
		{"the session ended underneath", client.ErrSessionEnded, false},
		{"the client was killed", client.ErrKilled, false},
		{"a send already in flight", client.ErrSendInFlight, false},

		// THE CASE NOTHING COVERED BEFORE, and the reason this file exists.
		// client.write returns json.Marshal's error and conn.Write's error
		// bare — neither is a sentinel and neither is a server answer. The
		// original implementation scored both as refusals.
		{"a bare unrecognised error", errors.New("something nobody enumerated"), false},
		{"a wrapped unrecognised error", fmt.Errorf("client: send: %w", errors.New("use of closed network connection")), false},

		// The server answering, which is the only thing that counts.
		{"an error frame from the server", &client.SendError{}, true},
		{"a wrapped error frame", fmt.Errorf("client: send: %w", &client.SendError{}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendWasRefused(tc.err); got != tc.want {
				t.Errorf("sendWasRefused(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The asymmetry stated as its own claim, so it cannot be lost to a refactor
// that only keeps the table green: an unrecognised error must never be able to
// turn an all-unacked steady phase into a finding about the server.
func TestAnUnrecognisedSendErrorCannotProduceAServerFailure(t *testing.T) {
	if sendWasRefused(errors.New("a failure mode invented next year")) {
		t.Fatal("an unrecognised error counts as the server refusing. That is how a loaded " +
			"runner, or a marshal bug in the client, becomes verdict=server_failure — the " +
			"false finding this ticket exists to remove")
	}

	// And the other direction, so this is not satisfied by a function that
	// simply always returns false.
	if !sendWasRefused(&client.SendError{}) {
		t.Fatal("a real error frame is not counted as a refusal; classify would then never " +
			"return server_failure for a server that refuses every send")
	}
}
