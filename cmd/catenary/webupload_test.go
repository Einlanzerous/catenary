package main

// CANT-313 — the web round trip of an image upload, against a live server.
//
// `web/uploadroundtrip.ts` is bundled by `npm run build:upload` and run here
// under node, the way websmoke_test.go runs the smoke bundle: this file serves
// setup()'s router over Postgres and a media directory on a real TCP port with
// the fan-out listener beside it, mints the enrollment token the bundle logs in
// with, and then reads what the SERVER holds.
//
// The bundle picks the shared fixture (a JPEG carrying EXIF orientation 6 and a
// GPS block), composes it through startSession -> useTransport -> the outbox,
// and the REAL uploader POSTs it. What this file proves on the other side: the
// record arrived carrying the handle the server minted, its attachment URL
// serves an image with none of the GPS in its bytes, and its `&v=thumb` serves.
//
// THE NEGATIVE CONTROL runs the same compose over an outbox rebuilt with no
// uploader — a composition root that forgot the pass-through, which compiles —
// and the bundle itself asserts it fails with "Attachments can't be sent yet".
// Here it is also required that the server saw nothing: no upload row.
//
// Gated like websmoke_test.go: CATENARY_WEB_UPLOAD unset skips, set and missing
// fails. `npm run smoke:upload` builds the bundle and sets it; verify.sh runs it
// in the database lane through `npm run smoke`.

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/wire"
)

func webUploadBundle(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CATENARY_WEB_UPLOAD")
	if p == "" {
		t.Skip("CATENARY_WEB_UPLOAD not set; skipping the web upload round trip (`npm run smoke:upload` in web/ builds the bundle and sets it)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("CATENARY_WEB_UPLOAD=%s: %v — build it with `npm run build:upload` in web/", p, err)
	}
	if os.Getenv("CATENARY_TEST_DATABASE_URL") == "" {
		t.Fatal("CATENARY_WEB_UPLOAD is set and CATENARY_TEST_DATABASE_URL is not: the round trip needs a live server, and a live server needs a database")
	}
	return p
}

// runUploadBundle serves a fresh rig, runs the bundle in `mode`, and returns
// the rig, the helper device's bearer token and the bundle's stdout.
func runUploadBundle(t *testing.T, mode string) (*uploadRig, string) {
	t.Helper()
	bundle := webUploadBundle(t)

	dir := t.TempDir()
	ctx, pool, st, d := processFixtureWith(t, slog.New(slog.DiscardHandler), func(c *config.Config) {
		c.MediaDir = dir
		c.MediaURLKeys = "1=" + strings.Repeat("ab", 32)
	})
	ada := mkUser(ctx, t, pool, "ada", "Ada")
	bob := mkUser(ctx, t, pool, "bob", "Bob")
	group := mkGroup(ctx, t, pool, "A", ada, bob)
	r := &uploadRig{t: t, ctx: ctx, pool: pool, d: d, dir: dir, ada: ada, bob: bob, group: group}

	// A second device for Ada, enrolled here, to read back what the bundle's
	// device sent.
	helper, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, d.router, enrollRequest(t, helper.Plaintext, "reader"))
	if code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}
	var enrolled wire.EnrollResponse
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatal(err)
	}
	r.token = string(enrolled.AccessToken)

	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	live := &rig{t: t, ctx: ctx, pool: pool, d: d, base: srv.URL, since: dbNow(ctx, t, pool)}
	live.runListener()

	bundleToken, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs(filepath.Join(mediaFixtureDir, "photo-exif-gps.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "node", bundle)
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"CATENARY_UPLOAD_BASE_URL="+srv.URL,
		"CATENARY_UPLOAD_ENROLLMENT_TOKEN="+bundleToken.Plaintext,
		"CATENARY_UPLOAD_CONVERSATION_ID="+group.String(),
		"CATENARY_UPLOAD_FIXTURE="+fixture,
		"CATENARY_UPLOAD_MODE="+mode,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("node %s (%s) against %s: %v\n%s", bundle, mode, srv.URL, err, out.String())
	}
	t.Log(strings.TrimSpace(out.String()))
	return r, out.String()
}

func TestTheWebUploadRoundTripSendsAnImageTheServerServes(t *testing.T) {
	r, out := runUploadBundle(t, "positive")
	m := regexp.MustCompile(`RESULT (\{.*\})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the bundle printed no RESULT line:\n%s", out)
	}
	var res struct {
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal([]byte(m[1]), &res); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	code, raw := do(t, r.d.router, req)
	if code != http.StatusOK {
		t.Fatalf("GET /sync = %d: %s", code, raw)
	}
	var page struct {
		Messages []struct {
			ClientID    string `json:"client_id"`
			Attachments []struct {
				Kind     string `json:"kind"`
				URL      string `json:"url"`
				Filename string `json:"filename"`
				Width    int64  `json:"width"`
				Height   int64  `json:"height"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	var url string
	for _, msg := range page.Messages {
		if msg.ClientID != res.ClientID {
			continue
		}
		if len(msg.Attachments) != 1 || msg.Attachments[0].Kind != "image" {
			t.Fatalf("the record carries %+v; want one image", msg.Attachments)
		}
		a := msg.Attachments[0]
		url = a.URL
		// Orientation 6 is applied at ingest, so the stored image is upright
		// and the 32x24 original reads 24x32.
		if a.Width != 24 || a.Height != 32 {
			t.Errorf("recorded %dx%d; want 24x32 (orientation applied)", a.Width, a.Height)
		}
		if a.Filename != "photo-exif-gps.jpg" {
			t.Errorf("filename = %q", a.Filename)
		}
	}
	if url == "" {
		t.Fatalf("no record with client_id %s reached the server: %s", res.ClientID, raw)
	}

	rec := r.get(url, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/") {
		t.Fatalf("GET %s = %d %q", url, rec.Code, rec.Header().Get("Content-Type"))
	}
	served := rec.Body.Bytes()
	if _, err := jpeg.Decode(bytes.NewReader(served)); err != nil {
		t.Errorf("the served attachment is not a decodable JPEG: %v", err)
	}
	// The fixture carries an EXIF block with a GPS IFD; ingest re-encodes
	// without any metadata, so none of it may be in the bytes served.
	if bytes.Contains(served, []byte("Exif")) || bytes.Contains(served, []byte{0xFF, 0xE1}) {
		t.Errorf("the served image still carries an EXIF/APP1 segment")
	}
	if thumb := r.get(url+"&v=thumb", nil); thumb.Code != http.StatusOK || thumb.Body.Len() == 0 {
		t.Errorf("GET %s&v=thumb = %d", url, thumb.Code)
	}
}

func TestTheWebUploadPassThroughNegativeControlFailsWithoutTheUploader(t *testing.T) {
	r, _ := runUploadBundle(t, "negative")
	var n int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM uploads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d upload rows exist; an outbox with no uploader must reach the server with nothing", n)
	}
}

// CANT-314 — ruling 1 → B, end to end. The bundle's picker refuses a HEIC, an
// SVG and a generated oversize body before anything is composed; with that
// pre-check bypassed, this server's 415 and 413 fail the entry with the same
// words and RETRY and DELETE work. The server must have kept nothing.
func TestTheWebUploadPickerRefusesWhatTheServerWouldAndTheServerStillDoes(t *testing.T) {
	r, _ := runUploadBundle(t, "unsendable")
	var uploads, messages int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM uploads`).Scan(&uploads); err != nil {
		t.Fatal(err)
	}
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if uploads != 0 || messages != 0 {
		t.Errorf("the server holds %d uploads and %d messages; an unsendable file leaves neither", uploads, messages)
	}
}

// CANT-315 — ruling 0 → B, the live half. The bundle's recording path, with a
// scripted recorder standing in for the microphone (the shipped one is gated
// off until CANT-56), hands the real uploader webm bytes as recorded; the
// server must have minted a handle for a `kind=voice` upload and hold its row,
// with the bytes exactly as sent and nothing the client could have measured
// about them: peaks and the server-measured duration stay NULL until CANT-56.
func TestTheWebUploadVoiceRecordingReturnsAHandleAndARow(t *testing.T) {
	r, out := runUploadBundle(t, "voice")
	m := regexp.MustCompile(`RESULT (\{.*\})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the bundle printed no RESULT line:\n%s", out)
	}
	var res struct {
		Handle string   `json:"handle"`
		Bytes  int      `json:"bytes"`
		Keys   []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(m[1]), &res); err != nil {
		t.Fatal(err)
	}
	// The client's voice draft carries no peaks: its four fields plus the handle
	// the uploader set on it.
	if strings.Join(res.Keys, ",") != "blob,durationMs,kind,upload,uploadId" {
		t.Errorf("the client's voice attachment has fields %v", res.Keys)
	}
	var kind string
	var n int64
	var peaksNull, durationNull bool
	if err := r.pool.QueryRow(r.ctx,
		`SELECT kind, bytes, peaks IS NULL, duration_ms IS NULL FROM uploads WHERE id = $1`, res.Handle).
		Scan(&kind, &n, &peaksNull, &durationNull); err != nil {
		t.Fatalf("no uploads row for the handle %s: %v", res.Handle, err)
	}
	if kind != "voice" || n != int64(res.Bytes) {
		t.Errorf("row = kind %q, %d bytes; want voice, %d", kind, n, res.Bytes)
	}
	if !peaksNull || !durationNull {
		t.Errorf("peaks null=%v duration null=%v: nothing the client sent may have filled them", peaksNull, durationNull)
	}
}
