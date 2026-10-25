package main

// CANT-321 (CANT-308 row 2) end to end through setup(): a link preview card is
// bound to a message by the same single-use handle an image upload is, and
// comes back on REST, /sync and a socket with its fields and a derived, signed
// image_url that really serves the blob. A text-only card (a page with a title
// and no usable image, so a NULL storage_key) goes through every one of those
// paths too.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/media"
	"github.com/magos/catenary/internal/wire"
)

const (
	cardURL   = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	cardTitle = "Rick Astley - Never Gonna Give You Up (Official Video)"
)

// linkRig is the fanout rig with a media directory, so a card's image is a real
// blob behind a real signed URL.
func newLinkRig(t *testing.T) (*rig, string) {
	t.Helper()
	dir := t.TempDir()
	log := &recorder{}
	ctx, pool, st, d := processFixtureWith(t, slog.New(log), func(c *config.Config) {
		c.MediaDir = dir
		c.MediaURLKeys = "1=" + strings.Repeat("ab", 32)
	})
	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	// The 0014 down refuses while a link card exists, and the next test's reset
	// is MigrateDown to empty: every card this rig makes is removed when the
	// test ends, on the still-open pool (Cleanup runs last-added first).
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM attachments WHERE kind = 'link'`, `DELETE FROM uploads WHERE kind = 'link'`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("cleanup: %s: %v", q, err)
			}
		}
	})
	r := &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, log: log, base: srv.URL, since: dbNow(ctx, t, pool)}
	r.runListener()
	return r, dir
}

func (r *rig) putBlob(dir string, b []byte) string {
	r.t.Helper()
	ms, err := media.NewLocal(dir)
	if err != nil {
		r.t.Fatal(err)
	}
	key, _, err := ms.Put(r.ctx, bytes.NewReader(b), 1<<20)
	if err != nil {
		r.t.Fatal(err)
	}
	return key
}

// seedCard inserts a ready link upload; imageKey/thumbKey empty makes it a
// text-only card.
func (r *rig) seedCard(uploader uuid.UUID, imageKey, thumbKey string) uuid.UUID {
	r.t.Helper()
	id := uuid.New()
	var sk, tk *string
	var w, h *int
	var ph *string
	if imageKey != "" {
		sk, tk = &imageKey, &thumbKey
		w, h, ph = ptrOf(4), ptrOf(4), ptrOf("LKO2?U%2Tw=w]~RBVZRi};RPxuwH")
	}
	if _, err := r.pool.Exec(r.ctx, `
		INSERT INTO uploads (id, uploader, kind, storage_key, thumb_key, bytes, width, height, placeholder,
		                     link_url, link_title, link_description, link_site_name)
		VALUES ($1, $2, 'link', $3, $4, 0, $5, $6, $7, $8, $9, $10, $11)`,
		id, uploader, sk, tk, w, h, ph, cardURL, cardTitle, "The official video.", "YouTube"); err != nil {
		r.t.Fatal(err)
	}
	return id
}

func ptrOf[T any](v T) *T { return &v }

func (r *rig) restSend(token string, conv uuid.UUID, handle uuid.UUID) (int, []byte) {
	r.t.Helper()
	return do(r.t, r.d.router, authedRequest(r.t, "/conversations/"+conv.String()+"/messages", token,
		map[string]any{"client_id": uuid.NewString(), "text": "this one " + cardURL,
			"attachments": []map[string]string{{"kind": "link", "upload_id": handle.String()}}}))
}

func (r *rig) get(rawURL string) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.d.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, rawURL, nil))
	return rec
}

// ack reads frames until the ack, skipping the author's own fan-out echoes.
func (s *session) ack() (wire.ServerAck, bool) {
	s.t.Helper()
	for i := 0; i < 8; i++ {
		switch f := s.next().(type) {
		case wire.ServerAck:
			return f, true
		case wire.ServerMessageFrame, wire.ServerConversationFrame, wire.ServerUserFrame:
		default:
			return wire.ServerAck{}, false
		}
	}
	return wire.ServerAck{}, false
}

func linkOf(t *testing.T, m wire.Message) wire.LinkAttachment {
	t.Helper()
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(m.Attachments))
	}
	l, ok := m.Attachments[0].(wire.LinkAttachment)
	if !ok {
		t.Fatalf("attachment = %T, want a link card", m.Attachments[0])
	}
	return l
}

func TestALinkCardRoundTripsThroughRestSyncAndTheSocket(t *testing.T) {
	r, dir := newLinkRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	bob := mkUser(r.ctx, t, r.pool, "bob", "Bob")
	conv := mkGroup(r.ctx, t, r.pool, "A", ada, bob)
	adaTok := string(r.enroll(ada, "laptop").AccessToken)

	png, thumb := pngBytes(t, 200), pngBytes(t, 90)
	imageKey, thumbKey := r.putBlob(dir, png), r.putBlob(dir, thumb)

	// REST send of a card with an image.
	withImg := r.seedCard(ada, imageKey, thumbKey)
	code, raw := r.restSend(adaTok, conv, withImg)
	if code != http.StatusOK {
		t.Fatalf("send = %d: %s", code, raw)
	}
	var sent wire.Message
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	card := linkOf(t, sent)
	if card.URL != cardURL || card.Title == nil || *card.Title != cardTitle ||
		card.Description == nil || card.SiteName == nil || *card.SiteName != "YouTube" ||
		card.ImageWidth == nil || *card.ImageWidth != 4 || card.ImageHeight == nil || card.ImagePlaceholder == nil {
		t.Errorf("card = %+v", card)
	}
	if card.ImageURL == nil || !strings.HasPrefix(*card.ImageURL, "/media/"+imageKey+"?kid=1&sig=") {
		t.Fatalf("image_url = %v, want a signed /media/%s URL", card.ImageURL, imageKey)
	}

	// The derived URL serves the blob, and &v=thumb serves the thumbnail through
	// uploads_storage_key_idx: the lookup is by the image's key, not the card's.
	if rec := r.get(*card.ImageURL); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) ||
		rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("GET image_url = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := r.get(*card.ImageURL + "&v=thumb"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), thumb) {
		t.Errorf("GET image_url&v=thumb = %d", rec.Code)
	}

	// A text-only card, sent beside it.
	textOnly := r.seedCard(ada, "", "")
	code, raw = r.restSend(adaTok, conv, textOnly)
	if code != http.StatusOK {
		t.Fatalf("send text-only = %d: %s", code, raw)
	}
	var sentText wire.Message
	if err := json.Unmarshal(raw, &sentText); err != nil {
		t.Fatal(err)
	}
	tc := linkOf(t, sentText)
	if tc.URL != cardURL || tc.Title == nil || tc.ImageURL != nil || tc.ImageWidth != nil ||
		tc.ImageHeight != nil || tc.ImagePlaceholder != nil {
		t.Errorf("text-only card = %+v: it must carry its text and no image field", tc)
	}
	// On the wire the field is absent, not null and not an empty string.
	if strings.Contains(string(raw), `"image_url"`) {
		t.Errorf("a text-only card serialized an image_url: %s", raw)
	}

	// Both handles are spent.
	for _, h := range []uuid.UUID{withImg, textOnly} {
		if code, raw := r.restSend(adaTok, conv, h); code != http.StatusNotFound {
			t.Errorf("a second send with a consumed handle = %d: %s; want 404", code, raw)
		} else {
			var e wire.ServerError
			_ = json.Unmarshal(raw, &e)
			if e.Code != wire.ErrorCodeUploadNotFound {
				t.Errorf("code = %q, want upload_not_found", e.Code)
			}
		}
	}

	// /sync, as the OTHER member: the same two cards, byte for byte.
	bobTok := string(r.enroll(bob, "phone").AccessToken)
	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+bobTok)
	code, raw = do(t, r.d.router, req)
	if code != http.StatusOK {
		t.Fatalf("GET /sync = %d: %s", code, raw)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("a /sync page holding a text-only card: %v", err)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("sync messages = %d, want 2", len(page.Messages))
	}
	for i, want := range []wire.LinkAttachment{card, tc} {
		got := linkOf(t, page.Messages[i])
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Errorf("sync card %d differs from the REST one:\n got  %s\n want %s", i, a, b)
		}
	}

	// The socket: ada sends a third card over her socket, bob's open session
	// receives it with the same fields.
	adaS := r.open(ada, "desk")
	bobS := r.open(bob, "tablet")
	third := r.seedCard(ada, imageKey, thumbKey)
	adaS.send(wire.ClientSend{
		ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(conv.String()),
		Attachments: []wire.OutboundAttachment{{Kind: "link", UploadID: wire.Uuid(third.String())}},
	})
	if _, ok := adaS.ack(); !ok {
		t.Fatal("no ack for a card sent over the socket")
	}
	got := linkOf(t, bobS.message())
	if got.URL != cardURL || got.ImageURL == nil || *got.ImageURL != *card.ImageURL {
		t.Errorf("the socket card = %+v; want the same fields and the same derived image_url", got)
	}

	// A text-only card over the socket, and a stale handle over it.
	adaS.send(wire.ClientSend{
		ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(conv.String()),
		Attachments: []wire.OutboundAttachment{{Kind: "link", UploadID: wire.Uuid(r.seedCard(ada, "", "").String())}},
	})
	if _, ok := adaS.ack(); !ok {
		t.Fatal("no ack for a text-only card sent over the socket")
	}
	if tcg := linkOf(t, bobS.message()); tcg.ImageURL != nil || tcg.Title == nil {
		t.Errorf("the socket text-only card = %+v", tcg)
	}
	adaS.send(wire.ClientSend{
		ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(conv.String()),
		Attachments: []wire.OutboundAttachment{{Kind: "link", UploadID: wire.Uuid(third.String())}},
	})
	var e wire.ServerError
	var ok bool
	for i := 0; i < 8 && !ok; i++ {
		f := adaS.next()
		if e, ok = f.(wire.ServerError); !ok {
			if _, isAck := f.(wire.ServerAck); isAck {
				break
			}
		}
	}
	if !ok || e.Code != wire.ErrorCodeUploadNotFound {
		t.Errorf("a stale handle over the socket = %T %+v, want upload_not_found", e, e)
	}
}

// With a text-only card in the table, the media route still serves other rows
// and still answers 404 for an unknown key; ?v=thumb never falls back.
func TestTheMediaRouteBesideATextOnlyCard(t *testing.T) {
	r, dir := newLinkRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	conv := mkGroup(r.ctx, t, r.pool, "A", ada)
	tok := string(r.enroll(ada, "laptop").AccessToken)

	// A text-only card sent, so a NULL storage_key is in both tables.
	if code, raw := r.restSend(tok, conv, r.seedCard(ada, "", "")); code != http.StatusOK {
		t.Fatalf("send = %d: %s", code, raw)
	}

	png := pngBytes(t, 120)
	key := r.putBlob(dir, png)
	id := uuid.New()
	if _, err := r.pool.Exec(r.ctx, `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, width, height, placeholder, filename)
		VALUES ($1, $2, 'image', $3, $4, 4, 4, 'L', 'x.png')`, id, ada, key, len(png)); err != nil {
		t.Fatal(err)
	}
	code, raw := do(t, r.d.router, authedRequest(t, "/conversations/"+conv.String()+"/messages", tok,
		map[string]any{"client_id": uuid.NewString(), "attachments": []map[string]string{{"kind": "image", "upload_id": id.String()}}}))
	if code != http.StatusOK {
		t.Fatalf("image send = %d: %s", code, raw)
	}
	var m wire.Message
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Attachments) != 1 {
		t.Fatalf("decode: %v", err)
	}
	img := m.Attachments[0].(wire.ImageAttachment)
	if rec := r.get(img.URL); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("GET an image beside a text-only card = %d", rec.Code)
	}
	if rec := r.get(img.URL + "&v=thumb"); rec.Code != http.StatusNotFound {
		t.Errorf("v=thumb with no thumbnail = %d, want 404", rec.Code)
	}

	signer, err := media.ParseKeys("1=" + strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]string{
		"an unknown key":            signer.URL(strings.Repeat("0", 64)),
		"an unknown key, thumb":     signer.URL(strings.Repeat("0", 64)) + "&v=thumb",
		"the empty-string key slot": signer.URL(""),
	} {
		if _, err := url.Parse(u); err != nil {
			t.Fatal(err)
		}
		if rec := r.get(u); rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want a refusal and never a 200 or a 500", name, rec.Code)
		}
	}
	if rec := r.get(signer.URL(strings.Repeat("0", 64))); rec.Code != http.StatusNotFound {
		t.Errorf("a validly signed unknown key = %d, want 404", rec.Code)
	}
}
