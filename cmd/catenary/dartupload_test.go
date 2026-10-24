package main

// CANT-316 (CANT-247) — the Dart client's image upload, end to end on a live
// server, through the Dart driver.
//
// THE DRIVER IS THE DART COMPOSITION ROOT THE RIGS CAN REACH: it builds the
// outbox the way app/lib/store/session.dart does, with the real HttpUploader
// over the store the media sits in. A JPEG that carries an EXIF orientation
// and a GPS block is composed from a file on disk, uploaded by the real
// uploader to the real POST /uploads, sent with the handle the server minted,
// acked, and arrives at a second account as a record whose signed /media/ URL
// serves the re-encoded image — with no EXIF in its bytes — and whose
// &v=thumb serves too.
//
// THE NEGATIVE CONTROL removes the pass-through: with
// CATENARY_DRIVER_NO_UPLOADER=1 the driver opens its outbox with the default
// uploader, and the same image must fail with "Attachments can't be sent yet"
// and never arrive. A pass-through that is forgotten compiles; this is what
// fails.
//
// Gated like dartlanes_test.go: CATENARY_DART_DRIVER unset skips; no database
// skips with the rig.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/media"
	"github.com/magos/catenary/internal/wire"
)

type dartUploadRig struct {
	*rig
	ada, bob   uuid.UUID
	adaDev     wire.EnrollResponse
	bobDev     wire.EnrollResponse
	group      uuid.UUID
	driver     *tsDriver
	ctx        context.Context
	fixtureJPG string
	mediaDir   string
}

func newDartUploadRig(t *testing.T) *dartUploadRig {
	t.Helper()
	dartDriverExe(t) // skips when there is no driver
	log := &recorder{}
	mediaDir := t.TempDir()
	ctx, pool, st, d := processFixtureWith(t, slog.New(log), func(c *config.Config) {
		c.MediaDir = mediaDir
		c.MediaURLKeys = "1=" + strings.Repeat("ab", 32)
	})
	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	r := &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, log: log, base: srv.URL, since: dbNow(ctx, t, pool)}
	r.runListener()

	ada := mkUser(ctx, t, pool, "ada", "Ada")
	bob := mkUser(ctx, t, pool, "bob", "Bob")
	group := mkGroup(ctx, t, pool, "A", ada, bob)
	fixture, err := filepath.Abs("../../testdata/media/photo-exif-gps.jpg")
	if err != nil {
		t.Fatal(err)
	}
	u := &dartUploadRig{rig: r, ada: ada, bob: bob, group: group, ctx: ctx, fixtureJPG: fixture, mediaDir: mediaDir}
	u.adaDev = r.enroll(ada, "Ada's phone")
	u.bobDev = r.enroll(bob, "Bob's phone")
	u.driver = dartLane.newClient(t, r.base, u.adaDev, client.Faults{})
	tsRun(t, ctx, u.driver)
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := u.driver.Await(waitCtx, func() bool { return u.driver.Status().Ready }); err != nil {
		t.Fatalf("the Dart client never became ready: %v", err)
	}
	return u
}

func (u *dartUploadRig) composeImage() wire.Uuid {
	u.t.Helper()
	var out struct {
		ClientID wire.Uuid `json:"clientId"`
	}
	if err := u.driver.call(u.ctx, "composeImage", map[string]any{
		"conversationId": u.group, "path": u.fixtureJPG, "filename": "harbour.jpg",
	}, &out); err != nil {
		u.t.Fatalf("composeImage: %v", err)
	}
	return out.ClientID
}

type driverEntry struct {
	ClientID wire.Uuid `json:"clientId"`
	State    string    `json:"state"`
	Error    string    `json:"error"`
}

func (u *dartUploadRig) entries() []driverEntry {
	u.t.Helper()
	var out struct {
		Entries []driverEntry `json:"entries"`
	}
	if err := u.driver.call(u.ctx, "outbox", nil, &out); err != nil {
		u.t.Fatalf("outbox: %v", err)
	}
	return out.Entries
}

// bobsImageURLs is every attachment URL on Bob's side of /sync.
func (u *dartUploadRig) bobsImageURLs() []string {
	var page struct {
		Messages []servedURLs `json:"messages"`
	}
	req, _ := http.NewRequest(http.MethodGet, u.base+"/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+string(u.bobDev.AccessToken))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(raw, &page); err != nil {
		u.t.Fatalf("decode /sync: %v", err)
	}
	var urls []string
	for _, m := range page.Messages {
		for _, a := range m.Attachments {
			urls = append(urls, a.URL)
		}
	}
	return urls
}

func (u *dartUploadRig) get(path string) (int, string, []byte) {
	u.t.Helper()
	res, err := http.Get(u.base + path)
	if err != nil {
		u.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), b
}

func TestTheDartClientUploadsAnImageAndASecondAccountReceivesIt(t *testing.T) {
	u := newDartUploadRig(t)
	original, err := os.ReadFile(u.fixtureJPG)
	if err != nil || !bytes.Contains(original, []byte("Exif")) {
		t.Fatalf("the fixture must carry EXIF (%v)", err)
	}

	u.composeImage()

	deadline := time.Now().Add(30 * time.Second)
	var urls []string
	for time.Now().Before(deadline) {
		if urls = u.bobsImageURLs(); len(urls) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(urls) != 1 {
		t.Fatalf("Bob received %d attachments; want 1 (driver outbox: %+v)", len(urls), u.entries())
	}
	if !strings.HasPrefix(urls[0], "/media/") {
		t.Fatalf("attachment url = %q; want the signed /media/ URL", urls[0])
	}
	code, ctype, body := u.get(urls[0])
	if code != http.StatusOK || ctype != "image/jpeg" {
		t.Fatalf("GET %s = %d %q", urls[0], code, ctype)
	}
	if bytes.Contains(body, []byte("Exif")) {
		t.Error("the served image still carries EXIF: the orientation and GPS block must be gone")
	}
	if code, _, _ := u.get(urls[0] + "&v=thumb"); code != http.StatusOK {
		t.Errorf("GET &v=thumb = %d", code)
	}
}

func TestWithoutThePassThroughTheDartClientCannotSendAnImage(t *testing.T) {
	t.Setenv("CATENARY_DRIVER_NO_UPLOADER", "1")
	u := newDartUploadRig(t)

	id := u.composeImage()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range u.entries() {
			if e.ClientID == id && e.State == "failed" {
				if e.Error != "Attachments can't be sent yet" {
					t.Fatalf("failed with %q; want the RefusingUploader's words", e.Error)
				}
				if urls := u.bobsImageURLs(); len(urls) != 0 {
					t.Fatalf("Bob received %v with no uploader", urls)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the entry never failed: %+v", u.entries())
}

// CANT-318 — the voice path the app's recorder feeds, proven against a live
// server while the app keeps REC disabled (CANT-247 ruling 0 → B). A recording
// is composed from a file as the platform recorder would leave it, the real
// uploader POSTs it as kind=voice, and the server mints a handle: a row exists
// with the recorded bytes stored as they were. The message then cannot be sent
// (a voice row resolves only with peaks, which CANT-56 computes), so the entry
// fails and Bob receives nothing: that end-to-end half is CANT-56's to prove,
// and the gate that keeps REC off in the app is why a person never sees it.
func TestTheDartClientsVoiceUploadMintsAHandleAndStoresTheBytesAsRecorded(t *testing.T) {
	u := newDartUploadRig(t)
	// An m4a: an ftyp box with the "M4A " brand, then bytes that are not text.
	recorded := append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'M', '4', 'A', ' ', 0, 0, 0, 0}, bytes.Repeat([]byte{0x9a, 0x01, 0xfe}, 400)...)
	path := filepath.Join(t.TempDir(), "take.m4a")
	if err := os.WriteFile(path, recorded, 0o600); err != nil {
		t.Fatal(err)
	}
	var out struct {
		ClientID wire.Uuid `json:"clientId"`
	}
	if err := u.driver.call(u.ctx, "composeVoice", map[string]any{"conversationId": u.group, "path": path, "durationMs": 4200}, &out); err != nil {
		t.Fatalf("composeVoice: %v", err)
	}

	var key string
	var kind string
	var n int64
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := u.pool.QueryRow(u.ctx, `SELECT kind, storage_key, bytes FROM uploads WHERE uploader = $1 ORDER BY created_at LIMIT 1`, u.ada).Scan(&kind, &key, &n)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no uploads row for Ada after the recording was composed: %v (driver outbox: %+v)", err, u.entries())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if kind != "voice" {
		t.Errorf("kind = %q; want voice", kind)
	}
	if n != int64(len(recorded)) {
		t.Errorf("bytes = %d; want %d", n, len(recorded))
	}
	stored, err := os.ReadFile(filepath.Join(u.mediaDir, "blobs", key[:2], key))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, recorded) {
		t.Error("the stored bytes are not the recorded bytes: voice is submitted and stored as recorded")
	}
	if ct, _ := media.ContentType(stored, "voice"); ct != "audio/mp4" {
		t.Errorf("the stored recording sniffs as %q; want audio/mp4", ct)
	}
	var withPeaks int
	if err := u.pool.QueryRow(u.ctx, `SELECT count(*) FROM uploads WHERE uploader = $1 AND peaks IS NOT NULL`, u.ada).Scan(&withPeaks); err != nil {
		t.Fatal(err)
	}
	if withPeaks != 0 {
		t.Errorf("%d upload rows carry peaks: no client computes them", withPeaks)
	}
	// The send is refused (upload_not_found), the outbox spends its one
	// re-upload, and the entry fails. Wait for that, then look at Bob.
	failedBy := time.Now().Add(20 * time.Second)
	for {
		var failed bool
		for _, e := range u.entries() {
			failed = failed || (e.ClientID == out.ClientID && e.State == "failed")
		}
		if failed {
			break
		}
		if time.Now().After(failedBy) {
			t.Fatalf("the unsendable voice entry never failed: %+v", u.entries())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if urls := u.bobsImageURLs(); len(urls) != 0 {
		t.Errorf("Bob received %v: a voice note cannot be sent until CANT-56", urls)
	}
}
