package main

// CANT-309's Done-when, end to end through the REAL router and store setup()
// builds, over Postgres and a media directory on disk.
//
// THROUGH setup() AND NOT A STORE THE TEST BUILDS: store.New defaults to a
// resolver that refuses every attachment, and forgetting WithUploadResolver
// compiles. These tests send through d.router, so a composition root that
// forgot the wiring fails here and not at the first photo in production.
//
// IMAGE ROWS ARE SEEDED BY SQL, in the shape CANT-310's ingest will write: the
// route refuses kind=image until that ingest exists, so there is no other way
// to hold a ready image handle.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/wire"
)

type uploadRig struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	d     deps
	dir   string
	token string // ada's
	ada   uuid.UUID
	bob   uuid.UUID
	group uuid.UUID
}

func newUploadRig(t *testing.T) *uploadRig {
	t.Helper()
	dir := t.TempDir()
	ctx, pool, st, d := processFixtureWith(t, slog.New(slog.DiscardHandler), func(c *config.Config) {
		c.MediaDir = dir
		c.MediaURLKeys = "1=" + strings.Repeat("ab", 32)
		c.MaxVoiceBytes = 64
	})
	ada := mkUser(ctx, t, pool, "ada", "Ada")
	bob := mkUser(ctx, t, pool, "bob", "Bob")
	group := mkGroup(ctx, t, pool, "A", ada, bob)
	issued, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, d.router, enrollRequest(t, issued.Plaintext, "laptop"))
	if code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}
	var enrolled wire.EnrollResponse
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatal(err)
	}
	return &uploadRig{t: t, ctx: ctx, pool: pool, d: d, dir: dir, token: string(enrolled.AccessToken), ada: ada, bob: bob, group: group}
}

func (r *uploadRig) upload(token, query string, body []byte) (int, map[string]string) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/uploads"+query, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	code, raw := do(r.t, r.d.router, req)
	out := map[string]string{}
	_ = json.Unmarshal(raw, &out)
	return code, out
}

// seedImage inserts a READY image upload for uploader, as CANT-310 will.
func (r *uploadRig) seedImage(uploader uuid.UUID) uuid.UUID {
	r.t.Helper()
	id := uuid.New()
	if _, err := r.pool.Exec(r.ctx, `
		INSERT INTO uploads (id, uploader, kind, storage_key, bytes, width, height, placeholder, filename)
		VALUES ($1, $2, 'image', $3, 1234, 640, 480, 'LKO2?U%2Tw=w]~RBVZRi};RPxuwH', 'harbour.jpg')`,
		id, uploader, hex.EncodeToString(bytes.Repeat([]byte{byte(id[0])}, 32))); err != nil {
		r.t.Fatalf("seed image: %v", err)
	}
	return id
}

func (r *uploadRig) send(token string, attachments ...map[string]string) (int, []byte) {
	r.t.Helper()
	code, raw := do(r.t, r.d.router, authedRequest(r.t, "/conversations/"+r.group.String()+"/messages", token,
		map[string]any{"client_id": uuid.NewString(), "attachments": attachments}))
	return code, raw
}

func att(kind string, id uuid.UUID) map[string]string {
	return map[string]string{"kind": kind, "upload_id": id.String()}
}

func (r *uploadRig) consumed(id uuid.UUID) bool {
	r.t.Helper()
	var at *string
	if err := r.pool.QueryRow(r.ctx, `SELECT consumed_at::text FROM uploads WHERE id = $1`, id).Scan(&at); err != nil {
		r.t.Fatal(err)
	}
	return at != nil
}

func TestAVoiceUploadIsStoredAndMintsAHandle(t *testing.T) {
	r := newUploadRig(t)
	audio := []byte("opus as recorded")

	code, out := r.upload(r.token, "?kind=voice", audio)
	if code != http.StatusCreated {
		t.Fatalf("POST /uploads = %d: %v", code, out)
	}
	handle, err := uuid.Parse(out["upload_id"])
	if err != nil {
		t.Fatalf("upload_id %q is not a uuid", out["upload_id"])
	}

	sum := sha256.Sum256(audio)
	key := hex.EncodeToString(sum[:])
	stored, err := os.ReadFile(filepath.Join(r.dir, "blobs", key[:2], key))
	if err != nil {
		t.Fatalf("the blob is not at blobs/<hh>/<sha256>: %v", err)
	}
	if !bytes.Equal(stored, audio) {
		t.Fatal("the stored bytes are not the uploaded bytes (voice is stored as recorded)")
	}

	var uploader uuid.UUID
	var kind, storageKey string
	var n int64
	if err := r.pool.QueryRow(r.ctx, `SELECT uploader, kind, storage_key, bytes FROM uploads WHERE id = $1`, handle).
		Scan(&uploader, &kind, &storageKey, &n); err != nil {
		t.Fatalf("no uploads row for the handle: %v", err)
	}
	if uploader != r.ada || kind != "voice" || storageKey != key || n != int64(len(audio)) {
		t.Fatalf("row = %s %s %s %d", uploader, kind, storageKey, n)
	}
}

func TestUploadRefusals(t *testing.T) {
	r := newUploadRig(t)

	if code, _ := r.upload("", "?kind=voice", []byte("x")); code != http.StatusUnauthorized {
		t.Errorf("no credential = %d; want 401", code)
	}
	if code, _ := r.upload("not-a-token", "?kind=voice", []byte("x")); code != http.StatusUnauthorized {
		t.Errorf("bad credential = %d; want 401", code)
	}
	// image is refused until CANT-310's ingest exists: stored as uploaded it
	// would keep its EXIF.
	if code, _ := r.upload(r.token, "?kind=image", []byte("x")); code != http.StatusUnsupportedMediaType {
		t.Errorf("kind=image = %d; want 415", code)
	}
	if code, _ := r.upload(r.token, "?kind=video", []byte("x")); code != http.StatusUnsupportedMediaType {
		t.Errorf("unknown kind = %d; want 415", code)
	}
	if code, _ := r.upload(r.token, "", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("no kind = %d; want 400", code)
	}

	// The cap here is 64 bytes. Exactly 64 is accepted, 65 is not.
	if code, _ := r.upload(r.token, "?kind=voice", bytes.Repeat([]byte{1}, 64)); code != http.StatusCreated {
		t.Errorf("exactly the cap = %d; want 201", code)
	}
	over := bytes.Repeat([]byte{2}, 65)
	if code, _ := r.upload(r.token, "?kind=voice", over); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the cap = %d; want 413", code)
	}
	// A body that does not declare its length cannot slip past the check on
	// Content-Length: the streaming cap catches it.
	req := httptest.NewRequest(http.MethodPost, "/uploads?kind=voice", bytes.NewReader(bytes.Repeat([]byte{3}, 65)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+r.token)
	if code, _ := do(t, r.d.router, req); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the cap with no Content-Length = %d; want 413", code)
	}

	// None of the refusals stored anything: one upload (the exactly-64 one), one file.
	var rows int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM uploads`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("uploads rows = %d; want 1", rows)
	}
	var files int
	_ = filepath.WalkDir(filepath.Join(r.dir, "blobs"), func(_ string, e os.DirEntry, _ error) error {
		if e != nil && !e.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("blob files = %d; want 1", files)
	}
	if es, _ := os.ReadDir(filepath.Join(r.dir, "tmp")); len(es) != 0 {
		t.Errorf("tmp/ holds %d entries after the refusals", len(es))
	}
}

func TestAnAttachmentSendThroughTheCompositionRootConsumesTheHandle(t *testing.T) {
	r := newUploadRig(t)
	handle := r.seedImage(r.ada)

	code, raw := r.send(r.token, att("image", handle))
	if code != http.StatusOK {
		t.Fatalf("send with a ready handle = %d: %s", code, raw)
	}
	var m wire.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %d; want 1", len(m.Attachments))
	}
	if !r.consumed(handle) {
		t.Error("the handle is not consumed after the send")
	}

	// Single use: the same handle in a later send is not found.
	code, raw = r.send(r.token, att("image", handle))
	if code != http.StatusNotFound {
		t.Fatalf("a second send with the same handle = %d: %s; want 404", code, raw)
	}
	var e wire.ServerError
	_ = json.Unmarshal(raw, &e)
	if e.Code != wire.ErrorCodeUploadNotFound {
		t.Errorf("code = %q; want upload_not_found", e.Code)
	}
}

func TestOneHandleListedTwiceInOneSendIsRefusedAndNotConsumed(t *testing.T) {
	r := newUploadRig(t)
	handle := r.seedImage(r.ada)
	code, raw := r.send(r.token, att("image", handle), att("image", handle))
	if code != http.StatusNotFound {
		t.Fatalf("a handle listed twice = %d: %s; want 404", code, raw)
	}
	if r.consumed(handle) {
		t.Error("a refused send consumed its handle: the transaction should have rolled back")
	}
	if code, raw := r.send(r.token, att("image", handle)); code != http.StatusOK {
		t.Fatalf("the handle should still be sendable once = %d: %s", code, raw)
	}
}

func TestAnotherUsersHandleIsRefusedLikeAnUnknownOne(t *testing.T) {
	r := newUploadRig(t)
	bobs := r.seedImage(r.bob)
	code, _ := r.send(r.token, att("image", bobs))
	if code != http.StatusNotFound {
		t.Fatalf("ada sending bob's handle = %d; want 404", code)
	}
	if r.consumed(bobs) {
		t.Error("bob's handle was consumed by ada's refused send")
	}
	unknown := uuid.New()
	if c2, _ := r.send(r.token, att("image", unknown)); c2 != code {
		t.Errorf("an unknown handle = %d, bob's = %d: they must be indistinguishable", c2, code)
	}
}

func TestAHandleSentAsTheWrongKindIsRefused(t *testing.T) {
	r := newUploadRig(t)
	handle := r.seedImage(r.ada)
	if code, _ := r.send(r.token, att("voice", handle)); code != http.StatusNotFound {
		t.Fatalf("an image handle sent as voice = %d; want 404", code)
	}
	if r.consumed(handle) {
		t.Error("a wrong-kind send consumed the handle")
	}
}

// A voice upload with no peaks cannot become an attachment (migration 0003's
// attachments_voice_fields), and peaks are CANT-56's. Until they exist the
// handle resolves to nothing — and is NOT burned, so it works once they do.
func TestAVoiceHandleWithoutPeaksIsRefusedWithoutBeingConsumed(t *testing.T) {
	r := newUploadRig(t)
	code, out := r.upload(r.token, "?kind=voice", []byte("a voice note"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	handle := uuid.MustParse(out["upload_id"])

	if code, raw := r.send(r.token, att("voice", handle)); code != http.StatusNotFound {
		t.Fatalf("a voice handle with no peaks = %d: %s; want 404", code, raw)
	}
	if r.consumed(handle) {
		t.Fatal("a refused voice send burned the handle")
	}

	// What CANT-56 will do: fill peaks and duration.
	if _, err := r.pool.Exec(r.ctx, `UPDATE uploads SET peaks = '{0,50,100}', duration_ms = 2100 WHERE id = $1`, handle); err != nil {
		t.Fatal(err)
	}
	code, raw := r.send(r.token, att("voice", handle))
	if code != http.StatusOK {
		t.Fatalf("a voice handle once peaks exist = %d: %s", code, raw)
	}
	var m wire.Message
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Attachments) != 1 {
		t.Fatalf("decode: %v, %d attachments", err, len(m.Attachments))
	}
}

// With no CATENARY_MEDIA_DIR the process has no media store: no route, and the
// refusing resolver. A deployment that predates SERV-244 keeps booting.
func TestWithoutAMediaDirThereIsNoUploadRoute(t *testing.T) {
	_, _, _, d := processFixture(t, slog.New(slog.DiscardHandler))
	req := httptest.NewRequest(http.MethodPost, "/uploads?kind=voice", bytes.NewReader([]byte("x")))
	req.Header.Set("Authorization", "Bearer x")
	if code, _ := do(t, d.router, req); code == http.StatusCreated || code == http.StatusUnauthorized {
		t.Fatalf("POST /uploads = %d with no media dir; want the route absent (404/405)", code)
	}
}

// CANT-85 contract line 3, the reason the consume is one row-locked UPDATE: two
// sends naming one handle at once must produce exactly one message, and the
// loser must be refused upload_not_found, not an internal error.
func TestTwoConcurrentSendsOfOneHandleYieldExactlyOneMessage(t *testing.T) {
	r := newUploadRig(t)
	const rounds = 8
	for i := 0; i < rounds; i++ {
		handle := r.seedImage(r.ada)
		codes := make(chan int, 2)
		start := make(chan struct{})
		for j := 0; j < 2; j++ {
			go func() {
				<-start
				c, _ := r.send(r.token, att("image", handle))
				codes <- c
			}()
		}
		close(start)
		a, b := <-codes, <-codes
		if !((a == http.StatusOK && b == http.StatusNotFound) || (a == http.StatusNotFound && b == http.StatusOK)) {
			t.Fatalf("round %d: concurrent sends of one handle = %d and %d; want one 200 and one 404", i, a, b)
		}
	}
}
