package store

// CANT-129 ruling 2: `authentication refused` / `token expired` is at INFO, and
// nothing else moved.
//
// WHY A LEVEL IS WORTH A TEST. The four refusals Authenticate can log are one
// response — 401 with no detail — told apart only by their log lines, so the
// level is the whole of what a person sees first. Three of them are a credential
// being ADMINISTERED and belong at WARN. The fourth, an expired access token, is
// CANT-31's record §1 working as designed: the reactive refresh is "not
// optional", and a browser cannot see a failed upgrade's status, so meeting this
// refusal is how a well-behaved client learns to refresh. At WARN it was the
// loudest line a healthy estate produced, and CANT-129 is the ticket that
// measured how many of them one held device makes.
//
// AND WHY THE OTHER TWO `token expired` LINES ARE HERE TOO. The same string is
// logged by RotateRefresh for an expired REFRESH token and by RedeemEnrollment
// for an expired enrollment token, and both stay WARN: those credentials have
// ENDED, and the device needs a person. Somebody grepping `token expired` and
// finding one string at two levels should find this test rather than a puzzle.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestAnExpiredAccessTokenIsRefusedAtInfoAndTheAdministeredRefusalsStayWarn(t *testing.T) {
	ctx, pool := freshDB(t)

	// One store per case, so the buffer holds only that case's lines.
	newCase := func() (*Store, func() []map[string]any) {
		logger, buf := captureLogger()
		return New(pool, DefaultLimits(), logger), func() []map[string]any { return logLines(t, buf) }
	}

	for _, tc := range []struct {
		name, reason, level string
		// arrange returns the access token to present, having made exactly one
		// reason to refuse it true.
		arrange func(t *testing.T, st *Store) string
	}{
		{
			name: "an expired access token", reason: "token expired", level: "INFO",
			arrange: func(t *testing.T, st *Store) string {
				e := enrolled(ctx, t, st, pool, "ada-"+uuid.NewString()[:8])
				if _, err := pool.Exec(ctx,
					`UPDATE access_tokens SET expires_at = now() - interval '1 second' WHERE device_id = $1`,
					e.DeviceID); err != nil {
					t.Fatal(err)
				}
				return e.Access.Plaintext
			},
		},
		{
			name: "a revoked access token", reason: "token revoked", level: "WARN",
			arrange: func(t *testing.T, st *Store) string {
				e := enrolled(ctx, t, st, pool, "grace-"+uuid.NewString()[:8])
				if _, err := pool.Exec(ctx,
					`UPDATE access_tokens SET revoked_at = now() WHERE device_id = $1`, e.DeviceID); err != nil {
					t.Fatal(err)
				}
				return e.Access.Plaintext
			},
		},
		{
			name: "a revoked device", reason: "device revoked", level: "WARN",
			arrange: func(t *testing.T, st *Store) string {
				e := enrolled(ctx, t, st, pool, "linus-"+uuid.NewString()[:8])
				if _, err := pool.Exec(ctx,
					`UPDATE devices SET revoked_at = now() WHERE id = $1`, e.DeviceID); err != nil {
					t.Fatal(err)
				}
				return e.Access.Plaintext
			},
		},
		{
			name: "a deactivated account", reason: "account deactivated", level: "WARN",
			arrange: func(t *testing.T, st *Store) string {
				e := enrolled(ctx, t, st, pool, "victim-"+uuid.NewString()[:8])
				if _, err := pool.Exec(ctx,
					`UPDATE users SET deactivated_at = now() WHERE id = $1`, e.UserID); err != nil {
					t.Fatal(err)
				}
				return e.Access.Plaintext
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, lines := newCase()
			token := tc.arrange(t, st)
			// The arrangement logs too (an enrollment is a redemption); only what
			// happens next is asserted, so take a mark.
			before := len(lines())

			if _, err := st.Authenticate(ctx, token); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("the refusal = %v, want ErrUnauthorized — the STATUS is unchanged by this ticket", err)
			}

			after := lines()[before:]
			var matched int
			for _, l := range after {
				if l["msg"] != "authentication refused" || l["reason"] != tc.reason {
					continue
				}
				matched++
				if l["level"] != tc.level {
					t.Errorf("`%s` is logged at %v, want %s", tc.reason, l["level"], tc.level)
				}
				// THE QUIETER LEVEL COSTS NO DETAIL: the token id is what makes a
				// line answerable, and it is still there.
				if l["token_id"] == nil {
					t.Errorf("`%s` logged no token_id: %v", tc.reason, l)
				}
				// AND THE CREDENTIAL IS STILL NEVER IN A LINE, at any level.
				for k, v := range l {
					if s, ok := v.(string); ok && s == token {
						t.Errorf("the log line put the credential in field %q", k)
					}
				}
			}
			if matched != 1 {
				t.Errorf("%d lines said `authentication refused` with reason %q, want exactly 1: %v",
					matched, tc.reason, after)
			}
		})
	}

	// THE TWO OTHER `token expired` LINES, EACH STILL WARN. Different credentials,
	// both ended, both needing a person — which is the distinction the level now
	// carries on the access path.
	t.Run("an expired refresh token is refused at WARN", func(t *testing.T) {
		st, lines := newCase()
		e := enrolled(ctx, t, st, pool, "ada-refresh-"+uuid.NewString()[:8])
		if _, err := pool.Exec(ctx,
			`UPDATE refresh_tokens SET expires_at = now() - interval '1 second' WHERE device_id = $1`,
			e.DeviceID); err != nil {
			t.Fatal(err)
		}
		before := len(lines())
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("the refusal = %v, want ErrUnauthorized", err)
		}
		assertOneLineAt(t, lines()[before:], "refresh refused", "token expired", "WARN")
	})

	t.Run("an expired enrollment token is refused at WARN", func(t *testing.T) {
		st, lines := newCase()
		user := mkUser(ctx, t, pool, "invitee-"+uuid.NewString()[:8])
		issued, err := st.IssueEnrollmentToken(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE enrollment_tokens SET expires_at = now() - interval '1 second'
			  WHERE user_id = $1 AND redeemed_at IS NULL AND superseded_at IS NULL`, user); err != nil {
			t.Fatal(err)
		}
		before := len(lines())
		if _, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("the refusal = %v, want ErrUnauthorized", err)
		}
		assertOneLineAt(t, lines()[before:], "enrollment refused", "expired", "WARN")
	})

	// AND A LIVE TOKEN STILL AUTHENTICATES SILENTLY: the ticket lowered a level, it
	// did not add a line to the path that works.
	t.Run("a live access token authenticates and logs nothing", func(t *testing.T) {
		st, lines := newCase()
		e := enrolled(ctx, t, st, pool, "live-"+uuid.NewString()[:8])
		before := len(lines())
		if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
			t.Fatalf("a live token: %v", err)
		}
		for _, l := range lines()[before:] {
			if l["msg"] == "authentication refused" {
				t.Errorf("a live token logged a refusal: %v", l)
			}
		}
	})
}

// assertOneLineAt fails unless the captured lines carry exactly one record with
// this message and reason, at this level.
func assertOneLineAt(t *testing.T, lines []map[string]any, msg, reason, level string) {
	t.Helper()
	var matched int
	for _, l := range lines {
		if l["msg"] != msg || l["reason"] != reason {
			continue
		}
		matched++
		if l["level"] != level {
			t.Errorf("`%s` / `%s` is logged at %v, want %s", msg, reason, l["level"], level)
		}
	}
	if matched != 1 {
		t.Errorf("%d lines said `%s` with reason %q, want exactly 1: %v", matched, msg, reason, lines)
	}
}
