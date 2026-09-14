package main

// credentials.go's own comment claims this file checks wireTimestampLayout
// against internal/wire's TimestampPattern — this is that check.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/magos/catenary/internal/wire"
)

func TestWireTimestampLayoutMatchesTheWireSchemasPattern(t *testing.T) {
	got := time.Now().UTC().Format(wireTimestampLayout)
	if !wire.TimestampPattern.MatchString(got) {
		t.Fatalf("a time formatted with wireTimestampLayout (%q) does not match wire.TimestampPattern (%s)", got, wire.TimestampPattern)
	}
	// And the format actually carries milliseconds with a literal Z, not
	// merely something the pattern happens to also accept — the property the
	// comment on wireTimestampLayout is actually claiming.
	parsed, err := time.Parse(wireTimestampLayout, got)
	if err != nil {
		t.Fatalf("round-trip parse of %q failed: %v", got, err)
	}
	if !parsed.Equal(parsed.Truncate(time.Millisecond)) {
		t.Errorf("wireTimestampLayout lost or gained precision on round-trip: %v", parsed)
	}
}

func TestCredentialsFromEnrollmentRoundTripsTheWireTimestamps(t *testing.T) {
	access := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Millisecond)
	refresh := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	e := wire.EnrollResponse{
		UserID: "u", DeviceID: "d", AccessToken: "a", RefreshToken: "r",
		AccessExpiresAt:  wire.Timestamp(access.Format(wireTimestampLayout)),
		RefreshExpiresAt: wire.Timestamp(refresh.Format(wireTimestampLayout)),
	}
	cred, err := credentialsFromEnrollment(e)
	if err != nil {
		t.Fatalf("credentialsFromEnrollment: %v", err)
	}
	if !cred.AccessExpiresAt.Equal(access) {
		t.Errorf("AccessExpiresAt = %v, want %v", cred.AccessExpiresAt, access)
	}
	if !cred.RefreshExpiresAt.Equal(refresh) {
		t.Errorf("RefreshExpiresAt = %v, want %v", cred.RefreshExpiresAt, refresh)
	}

	// And writeCredentials/readCredentials preserve the same instant through
	// a real file, at 0600 — the other half of what this file is at rest.
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := writeCredentials(path, cred); err != nil {
		t.Fatalf("writeCredentials: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	back, err := readCredentials(path)
	if err != nil {
		t.Fatalf("readCredentials: %v", err)
	}
	if !back.AccessExpiresAt.Equal(access) || !back.RefreshExpiresAt.Equal(refresh) {
		t.Errorf("read-back timestamps = %+v, want access=%v refresh=%v", back, access, refresh)
	}
}
