package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/magos/catenary/internal/wire"
)

// Enroll redeems an enrollment token over POST /enroll and returns the pair it
// minted, WITH the server's Date and the device's clock offset captured from
// that response — CANT-31's record §1 names /enroll beside /refresh as the two
// moments an expiry is learned, and this is the /enroll one.
//
// IT MATTERS FOR THE WHOLE LIFE OF THE FIRST PAIR. A credential built from the
// decoded body alone (CredentialFromEnroll) has no offset and no issue time, so
// until its first rotation the proactive check reads an uncorrected clock
// against the 60 s floor: on a device an hour slow it never fires at all, and
// every dial in the pair's last minutes presents a token the server has
// already killed. The reactive path recovers that — it is what it is for — but
// the proactive half should not be inert from enrollment to first refresh.
//
// cfg supplies BaseURL, HTTPClient, ExtraHeaders and Now; nothing else in it is
// read, and its Journal is not touched — the caller enrolls the journal with
// what this returns.
func Enroll(ctx context.Context, cfg Config, enrollmentToken, deviceName string) (Credential, error) {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	body, err := json.Marshal(wire.EnrollRequest{EnrollmentToken: wire.Token(enrollmentToken), DeviceName: deviceName})
	if err != nil {
		return Credential{}, fmt.Errorf("client: enroll: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/enroll", bytes.NewReader(body))
	if err != nil {
		return Credential{}, fmt.Errorf("client: enroll: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range cfg.ExtraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("client: enroll: %w", err)
	}
	defer resp.Body.Close()
	arrived := now().Round(0)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if err != nil {
		return Credential{}, fmt.Errorf("client: enroll: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if len(raw) > 256 {
			raw = raw[:256]
		}
		return Credential{}, fmt.Errorf("client: enroll: %s: %s", resp.Status, raw)
	}
	var out wire.EnrollResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Credential{}, fmt.Errorf("client: enroll: decode: %w", err)
	}
	cred, err := CredentialFromEnroll(out)
	if err != nil {
		return Credential{}, err
	}
	// No usable Date: the pair is still good, and it is the uncorrected shape
	// described above — there is no earlier offset to fall back on, as a
	// refresh has.
	if date, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		cred = cred.WithServerDate(date, arrived)
	}
	return cred, nil
}
