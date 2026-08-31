package catenary

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"

	"github.com/Einlanzerous/purser/spike-r6/fakecatenary"
)

func newFixture(t *testing.T, revokeFirst bool) (*Connector, *fakecatenary.Server) {
	t.Helper()
	fake := fakecatenary.New()
	srv := fake.Start()
	t.Cleanup(srv.Close)
	c, err := New(Config{
		BaseURL: srv.URL, AdminToken: "svc-token", AppURL: "https://catenary.example",
		RevokeDevicesFirst: revokeFirst,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, fake
}

// GATE QUESTION 1. The existing connectors mint one secret per person per
// service; Catenary's unit is a device. The contract wants one secret — and
// gets one, because at provisioning time the person has no devices yet.
func TestProvision_ReturnsExactlyOneEnrolmentToken(t *testing.T) {
	c, fake := newFixture(t, false)

	res, err := c.Provision(context.Background(), connector.Input{
		PersonName: "Nadia Ruiz", Email: "Nadia@Example.com",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Secret == "" || !strings.HasPrefix(res.Secret, "ctn_enrol_") {
		t.Errorf("expected a single enrolment token, got %q", res.Secret)
	}
	if res.ExternalID == "" {
		t.Error("expected an upstream account id")
	}
	// The shape of the answer is the finding: nothing had to be smuggled
	// through Extra, and there is no list anywhere in Result.
	if len(res.Extra) != 0 {
		t.Errorf("Result.Extra should be unnecessary, got %v", res.Extra)
	}
	if a := fake.Lookup("nadia@example.com"); a == nil {
		t.Fatal("account not created")
	} else if len(a.Devices) != 0 {
		t.Errorf("a freshly provisioned person must have no devices, got %d", len(a.Devices))
	}
}

// A re-invite must hand over something redeemable. The Lyceum-style "409 →
// success with no secret" branch would leave the person with nothing, because
// a single-use token that has already been redeemed cannot be handed out twice.
func TestProvision_ReissuesForAnExistingAccount(t *testing.T) {
	c, fake := newFixture(t, false)
	fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9", "ThinkPad")

	res, err := c.Provision(context.Background(), connector.Input{
		PersonName: "Nadia Ruiz", Email: "nadia@example.com",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.Contains(res.Secret, "reissued") {
		t.Errorf("expected a re-issued enrolment token, got %q", res.Secret)
	}
}

// GATE QUESTION 2. Reconcile is read-only and carries identity only. It cannot
// express "three devices, one revoked" — and must not try. What it MUST get
// right is that access is an account-level fact.
func TestReconcile_KeysOnAccountStatusNotRowExistence(t *testing.T) {
	ctx := context.Background()

	t.Run("active account with every device revoked still has access", func(t *testing.T) {
		c, fake := newFixture(t, false)
		a := fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9")
		a.Devices[0].Revoked = true

		got, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		// They can enrol a new device, so they have access. A connector that
		// counted live devices would report them as offboarded.
		if !got.Exists {
			t.Error("an active account with no live devices must still report access")
		}
	})

	t.Run("disabled account with un-revoked devices does not", func(t *testing.T) {
		c, fake := newFixture(t, false)
		a := fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9", "ThinkPad")
		a.Status = "disabled"

		got, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		// This is the trap. `Exists: row != nil` would say true here, and an
		// audit would then leave a disabled account looking healthy.
		if got.Exists {
			t.Error("a disabled account must not report access, however many device rows remain")
		}
	})

	t.Run("no account at all", func(t *testing.T) {
		c, _ := newFixture(t, false)
		got, err := c.Reconcile(ctx, connector.Input{Email: "nobody@example.com"})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got.Exists {
			t.Error("expected Exists=false")
		}
	})
}

// GATE QUESTION 3. The fan-out is fine. What it introduces is that Deprovision
// is no longer atomic upstream — and the ORDER of the two halves decides what a
// half-completed offboard leaves behind.
func TestDeprovision_DisablesTheAccountBeforeRevokingDevices(t *testing.T) {
	c, fake := newFixture(t, false)
	fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9", "ThinkPad")

	if err := c.Deprovision(context.Background(), connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	var disableAt, firstRevokeAt = -1, -1
	for i, call := range fake.Calls {
		if strings.HasSuffix(call, "/disable") && disableAt < 0 {
			disableAt = i
		}
		if strings.HasSuffix(call, "/revoke") && firstRevokeAt < 0 {
			firstRevokeAt = i
		}
	}
	if disableAt < 0 || firstRevokeAt < 0 {
		t.Fatalf("expected both a disable and a revoke, got calls: %v", fake.Calls)
	}
	if disableAt > firstRevokeAt {
		t.Errorf("disable must come first; calls were %v", fake.Calls)
	}

	a := fake.Lookup("nadia@example.com")
	if a.Status != "disabled" {
		t.Errorf("account status = %q, want disabled", a.Status)
	}
	for _, d := range a.Devices {
		if !d.Revoked {
			t.Errorf("device %s left un-revoked", d.ID)
		}
	}
}

// THE FINDING, stated as two tests that differ only in ordering.
//
// A device revoke fails partway through. In the correct order the person's
// access is already gone and Purser can see that it is gone. In the inverted
// order it is not, and a half-completed offboard is indistinguishable from one
// that never started.
func TestDeprovision_PartialFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("disable first: access is gone and Purser can see it", func(t *testing.T) {
		c, fake := newFixture(t, false)
		a := fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9", "ThinkPad")
		fake.FailRevokeDevice = a.Devices[1].ID

		err := c.Deprovision(ctx, connector.Input{Email: "nadia@example.com"})
		if err == nil {
			t.Fatal("expected the partial failure to surface as an error")
		}

		got, rerr := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if rerr != nil {
			t.Fatalf("Reconcile: %v", rerr)
		}
		if got.Exists {
			t.Error("after a partial deprovision the person must not report as having access")
		}

		// And the retry completes it, which is what makes leaving it half-done safe.
		fake.FailRevokeDevice = ""
		if err := c.Deprovision(ctx, connector.Input{Email: "nadia@example.com"}); err != nil {
			t.Fatalf("retry: %v", err)
		}
		for _, d := range fake.Lookup("nadia@example.com").Devices {
			if !d.Revoked {
				t.Errorf("device %s still un-revoked after retry", d.ID)
			}
		}
	})

	t.Run("devices first: the same failure leaves access intact and invisible", func(t *testing.T) {
		c, fake := newFixture(t, true) // the wrong order
		a := fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9", "ThinkPad")
		fake.FailRevokeDevice = a.Devices[1].ID

		err := c.Deprovision(ctx, connector.Input{Email: "nadia@example.com"})
		if err == nil {
			t.Fatal("expected the partial failure to surface as an error")
		}

		got, rerr := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if rerr != nil {
			t.Fatalf("Reconcile: %v", rerr)
		}
		// Not a bug in Reconcile — the account IS active, so the person really
		// can still enrol a device. That is exactly why the ordering matters:
		// the offboard failed open, and nothing in Purser's model can tell.
		if !got.Exists {
			t.Error("expected the wrong order to leave an active account (this test documents the hazard)")
		}
		if fake.Lookup("nadia@example.com").Status != "active" {
			t.Error("expected the account to still be active in the wrong order")
		}
	})
}

// The contract requires idempotency on the path you cannot take back.
func TestDeprovision_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	c, fake := newFixture(t, false)

	if err := c.Deprovision(ctx, connector.Input{Email: "ghost@example.com"}); err != nil {
		t.Errorf("deprovisioning a person with no account must be a success, got %v", err)
	}

	fake.Seed("nadia@example.com", "Nadia Ruiz", "Pixel 9")
	for i := range 3 {
		if err := c.Deprovision(ctx, connector.Input{Email: "nadia@example.com"}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// Catenary can revoke, so it must NOT advertise itself as unable — the offboard
// preview promises exactly what --apply will do.
func TestCanDeprovision_IsNotRefused(t *testing.T) {
	c, _ := newFixture(t, false)
	if err := connector.CanDeprovision(c); err != nil {
		t.Errorf("Catenary can revoke; CanDeprovision should be nil, got %v", err)
	}
	if connector.IsUnavailable(errors.New("x")) {
		t.Error("sanity: a plain error is not an unavailable sentinel")
	}
}
