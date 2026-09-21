package main

// CANT-130 — `catenary user set-email`, the one-time adoption path for a
// person soakrig created with no email. The store's own tests are the oracle
// for SetEmail's semantics; this file is the CLI wiring, on bot_test.go's
// own shape.

import (
	"strings"
	"testing"
)

func TestUserSetEmailGivesAnEmaillessPersonOne(t *testing.T) {
	ctx, pool, st, _ := authFixture(t)
	mkUser(ctx, t, pool, "alice", "Alice")

	if err := userSetEmail(ctx, st, []string{"alice", "alice@example.com"}); err != nil {
		t.Fatalf("user set-email: %v", err)
	}

	got, err := st.PersonByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("the new email does not resolve: %v", err)
	}
	if got.Handle != "alice" {
		t.Errorf("resolved handle = %q, want alice", got.Handle)
	}
}

func TestUserSetEmailRefusesABot(t *testing.T) {
	ctx, _, st, _ := authFixture(t)
	if _, err := st.CreateBot(ctx, "argosy", "Argosy"); err != nil {
		t.Fatal(err)
	}

	err := userSetEmail(ctx, st, []string{"argosy", "argosy@example.com"})
	if err == nil {
		t.Fatal("set-email on a bot succeeded")
	}
	if !strings.Contains(err.Error(), "bot") {
		t.Errorf("the error does not say why: %v", err)
	}
}

func TestUserSetEmailRefusesAnUnknownHandle(t *testing.T) {
	ctx, _, st, _ := authFixture(t)
	err := userSetEmail(ctx, st, []string{"nobody", "x@example.com"})
	if err == nil {
		t.Fatal("set-email on an unknown handle succeeded")
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("the error does not name the handle: %v", err)
	}
}

func TestUserSetEmailRefusesAnEmailAlreadyHeld(t *testing.T) {
	ctx, pool, st, _ := authFixture(t)
	if _, err := st.EnsurePerson(ctx, "ada@example.com", "Ada"); err != nil {
		t.Fatal(err)
	}
	mkUser(ctx, t, pool, "bob", "Bob")

	if err := userSetEmail(ctx, st, []string{"bob", "ada@example.com"}); err == nil {
		t.Fatal("set-email onto an already-held email succeeded")
	}
}

func TestUserSetEmailRefusesAPersonWhoAlreadyHasOne(t *testing.T) {
	ctx, _, st, _ := authFixture(t)
	ep, err := st.EnsurePerson(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := userSetEmail(ctx, st, []string{ep.Account.Handle, "someone-else@example.com"}); err == nil {
		t.Fatal("set-email overwrote an existing email")
	}
}

func TestUserSetEmailRefusesAMalformedEmail(t *testing.T) {
	ctx, pool, st, _ := authFixture(t)
	mkUser(ctx, t, pool, "alice", "Alice")
	if err := userSetEmail(ctx, st, []string{"alice", "not-an-email"}); err == nil {
		t.Fatal("a malformed email was accepted")
	}
}

func TestUserSetEmailRequiresAHandleAndAnEmail(t *testing.T) {
	ctx, _, st, _ := authFixture(t)
	if err := userSetEmail(ctx, st, nil); err == nil {
		t.Fatal("no arguments succeeded")
	}
	if err := userSetEmail(ctx, st, []string{"alice"}); err == nil {
		t.Fatal("a handle with no email succeeded")
	}
}

// The subcommand dispatch itself, on TestBotRejectsAnUnknownAction's shape.
func TestUserRejectsAnUnknownAction(t *testing.T) {
	if err := runUser([]string{"delete-everything"}); err == nil {
		t.Error("an unknown user action succeeded")
	} else if !strings.Contains(err.Error(), "delete-everything") {
		t.Errorf("the error does not name the action: %v", err)
	}
	if err := runUser(nil); err == nil {
		t.Error("`catenary user` with no action succeeded")
	}
}
