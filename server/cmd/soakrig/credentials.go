package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/magos/catenary/internal/wire"
)

// Credentials is exactly what an install receives from POST /enroll, at rest
// on disk: `provision` writes one, `idle` reads it back. Nothing else in
// this package persists a credential anywhere else, and neither command ever
// puts one on stdout — CANT-109's Done-when.
type Credentials struct {
	UserID           wire.Uuid `json:"user_id"`
	DeviceID         wire.Uuid `json:"device_id"`
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

// wireTimestampLayout mirrors internal/wireview.TimeLayout — RFC 3339 with
// milliseconds and a literal Z, the one shape internal/wire's own
// TimestampPattern accepts. Restated here, rather than importing
// internal/wireview for one layout string, so this small package does not
// take on that package's own dependency surface; credentials_test.go checks
// the two agree.
const wireTimestampLayout = "2006-01-02T15:04:05.000Z"

// credentialsFromEnrollment converts a real EnrollResponse — the ONLY source
// this package builds a Credentials from — into what gets written to disk.
func credentialsFromEnrollment(e wire.EnrollResponse) (Credentials, error) {
	accessExp, err := time.Parse(wireTimestampLayout, string(e.AccessExpiresAt))
	if err != nil {
		return Credentials{}, fmt.Errorf("parse access_expires_at %q: %w", e.AccessExpiresAt, err)
	}
	refreshExp, err := time.Parse(wireTimestampLayout, string(e.RefreshExpiresAt))
	if err != nil {
		return Credentials{}, fmt.Errorf("parse refresh_expires_at %q: %w", e.RefreshExpiresAt, err)
	}
	return Credentials{
		UserID: e.UserID, DeviceID: e.DeviceID,
		AccessToken: string(e.AccessToken), AccessExpiresAt: accessExp,
		RefreshToken: string(e.RefreshToken), RefreshExpiresAt: refreshExp,
	}, nil
}

// writeCredentials writes cred as JSON to path at mode 0600 and NEVER to
// stdout. os.WriteFile only applies perm to a file it creates — a rerun over
// a path that already exists at a looser mode would otherwise leave a
// credential file world-readable — so the mode is set again explicitly,
// unconditionally, after the write.
func writeCredentials(path string, cred Credentials) error {
	b, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write credentials to %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set mode 0600 on %s: %w", path, err)
	}
	return nil
}

// readCredentials is `idle`'s side of writeCredentials.
func readCredentials(path string) (Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read credentials from %s: %w", path, err)
	}
	var cred Credentials
	if err := json.Unmarshal(b, &cred); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials from %s: %w", path, err)
	}
	return cred, nil
}
