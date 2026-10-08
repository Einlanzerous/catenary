package main

// CANT-39 — the web smoke test, against a live server.
//
// `web/smoke.ts` server-side renders the app and asserts that the design
// canvas's landmarks are on screen. Its records used to come from
// `web/src/mock/fixtures.ts`; that file is gone, and this is where they come
// from instead: the canvas's corpus, seeded into a real Postgres through the
// store, served by the process setup() builds — the same composition root
// `catenary serve` runs — on a real TCP port, with the listener that fans
// out live frames running beside it.
//
// The bundle does the rest the way a browser would. It logs in through the
// login form's own `login()` against this server's POST /enroll, with an
// enrollment token minted by EnsurePerson (Purser's invite path); its
// `onEnrolled` listener starts the session; the TypeScript transport dials
// /ws, catches up over /sync, and CANT-35's projection puts the journal into
// `state`. Nothing between the seed and the render is scripted.
//
// SEEDED THROUGH THE STORE WHEREVER THE STORE HAS AN API: EnsurePerson for
// people, FindOrCreateDirect for directs, SendMessage for every message and
// reply, MarkRead for every receipt, DeactivateUser for the two offboarded
// people. Three things have no API yet and are written the way this package's
// other tests write them — a group room and its members (mkGroup; there is no
// room-creation path until CANT-33's connector), a room's `muted` flag (there
// is no mute action yet), and the attachment rows a send names, which come
// from an UploadResolver standing in for CANT-48's, exactly as
// servedattachments_test.go's does.
//
// Gated like tslanes_test.go: CATENARY_WEB_SMOKE unset skips, set and missing
// fails, and set with no database fails too — `npm run smoke` asked for a live
// render, and a skip would be a green line that rendered nothing. `npm run
// smoke` builds the bundle and sets the variable; verify.sh runs it in the
// database lane.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

func webSmokeBundle(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CATENARY_WEB_SMOKE")
	if p == "" {
		t.Skip("CATENARY_WEB_SMOKE not set; skipping the web smoke (`npm run smoke` in web/ builds the bundle and sets it)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("CATENARY_WEB_SMOKE=%s: %v — the smoke was asked to run and there is no bundle; build it with `npm run build:smoke` in web/", p, err)
	}
	if os.Getenv("CATENARY_TEST_DATABASE_URL") == "" {
		t.Fatal("CATENARY_WEB_SMOKE is set and CATENARY_TEST_DATABASE_URL is not: the smoke renders against a live server, and a live server needs a database")
	}
	return p
}

func TestTheWebSmokeRendersTheCanvasFromALiveServer(t *testing.T) {
	bundle := webSmokeBundle(t)
	r := newRig(t)
	c := seedCanvas(t, r)

	// A real peer: Nadia, on her own device and her own socket, typing in
	// Kitchen Table. The hub relays a `typing` list only to sessions attached
	// when it changes, and the app attaches whenever the bundle gets there, so
	// she re-announces until the test ends — each `start` re-emits the list.
	nadia := openAt(t, r.ctx, r.base, r.enroll(c.nadia, "Nadia's phone"), c.nadia)
	peer, stopPeer := context.WithCancel(r.ctx)
	defer stopPeer()
	go func() {
		for {
			if _, _, err := nadia.conn.Read(peer); err != nil {
				return
			}
		}
	}()
	go func() {
		start, _ := json.Marshal(wire.ClientTyping{ConversationID: wid(c.kitchen), State: wire.TypingStateStart})
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			if err := nadia.conn.Write(peer, websocket.MessageText, start); err != nil {
				return
			}
			select {
			case <-peer.Done():
				return
			case <-tick.C:
			}
		}
	}()

	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", bundle)
	// smoke.ts reads server/spec/testdata/read-fraction.json relative to web/.
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"CATENARY_SMOKE_BASE_URL="+r.base,
		"CATENARY_SMOKE_ENROLLMENT_TOKEN="+c.hollisToken,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("node %s against %s: %v — the assertions above say which", bundle, r.base, err)
	}
}

// canvas is what the bundle is handed and what the peer needs: Hollis's
// invitation, and the two ids Nadia's typing names.
type canvas struct {
	hollisToken string
	nadia       uuid.UUID
	kitchen     uuid.UUID
	// What the device run (devicerun_test.go) needs on top: the reader, and a
	// room and an author for the messages it adds.
	hollis uuid.UUID
	marek  uuid.UUID
	shed   uuid.UUID
}

// canvasUploads stands in for CANT-48's resolver, which does not exist yet:
// each handle the seed names resolves once, to the row prepared for it. It
// touches no table, so the contract's locking rule has nothing to lock.
type canvasUploads map[uuid.UUID]store.AttachmentRow

func (u canvasUploads) Resolve(_ context.Context, _ pgx.Tx, _ uuid.UUID, want []store.NewAttachment) ([]store.AttachmentRow, error) {
	out := make([]store.AttachmentRow, len(want))
	for i, w := range want {
		row, ok := u[w.UploadID]
		if !ok {
			return nil, fmt.Errorf("canvas: upload %s: %w", w.UploadID, store.ErrUploadNotFound)
		}
		delete(u, w.UploadID)
		out[i] = row
	}
	return out, nil
}

// canvasPeaks is the design canvas's waveform generator, run where peaks are
// always computed — on the server. In 64-bit integers it is exact; it is the
// JavaScript port of it that is not (web/src/lib/waveform.ts says why).
func canvasPeaks(seed int64, n int) []int64 {
	s := seed
	out := make([]int64, n)
	for i := range out {
		s = (s*1103515245 + 12345) % 2147483648
		r := float64(s) / 2147483648
		env := 0.35 + 0.65*math.Sin(float64(i)/float64(n-1)*math.Pi)
		v := int64(math.Round((0.22 + 0.78*r) * env * 100))
		out[i] = max(9, v)
	}
	return out
}

const ilseTranscript = "Okay so I talked to Ted about the delivery and the short version is Thursday " +
	"still works but they want everything staged by two, which means somebody has " +
	"to be at the shed in the morning to sign for the pallet. If nobody can do " +
	"that I’ll ask them to hold it until Friday, but then we’re into the " +
	"weekend and I’d rather not. Also the tension gauge came back from the " +
	"shop, it’s in the blue case on the bench."

// seedCanvas writes the design canvas's corpus: the people, the rooms and
// directs, every message in the order the canvas draws it, and the receipts
// that make its counts true rather than chosen.
func seedCanvas(t *testing.T, r *rig) canvas {
	t.Helper()
	ctx := r.ctx
	uploads := canvasUploads{}
	st := r.st.WithUploadResolver(uploads)

	type person struct {
		id     uuid.UUID
		handle string
	}
	people := map[string]person{}
	var hollisToken string
	for _, p := range []struct{ key, name string }{
		{"hollis", "Hollis Byrne"}, {"ilse", "Ilse Marchetti"}, {"nadia", "Nadia Okonkwo"},
		{"marek", "Marek Dubois"}, {"ted", "Ted Almasy"}, {"rosa", "Rosa Whitfield"},
		{"petra", "Petra Lindqvist"}, {"oskar", "Oskar Lindgren"}, {"wren", "Wren Castellano"},
		// Ines shares no room and no direct with Hollis: the one person on the
		// roster (GET /users) the new-conversation picker can CREATE a direct
		// with (CANT-270). Every other person already has one.
		{"ines", "Ines Calloway"},
	} {
		e, err := st.EnsurePerson(ctx, p.key+"@canvas.test", p.name)
		if err != nil {
			t.Fatalf("EnsurePerson(%s): %v", p.key, err)
		}
		people[p.key] = person{e.Account.UserID, e.Account.Handle}
		if p.key == "hollis" {
			hollisToken = e.Token.Plaintext
		}
	}
	id := func(key string) uuid.UUID { return people[key].id }
	me := id("hollis")

	room := func(name string, keys ...string) uuid.UUID {
		members := []uuid.UUID{me}
		for _, k := range keys {
			members = append(members, id(k))
		}
		return mkGroup(ctx, t, r.pool, name, members...)
	}
	direct := func(key string) uuid.UUID {
		c, err := st.FindOrCreateDirect(ctx, me, people[key].handle)
		if err != nil {
			t.Fatalf("FindOrCreateDirect(%s): %v", key, err)
		}
		return c.ID
	}

	// One send. `text` may be empty for an attachment-only message; `reply`
	// is the id of an earlier send in the same conversation, or uuid.Nil.
	say := func(conv uuid.UUID, author, text string, reply uuid.UUID, atts ...store.AttachmentRow) store.Sent {
		m := store.NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: id(author)}
		if text != "" {
			m.Text = &text
		}
		if reply != uuid.Nil {
			m.ReplyTo = &reply
		}
		for _, a := range atts {
			handle := uuid.New()
			uploads[handle] = a
			m.Attachments = append(m.Attachments, store.NewAttachment{Kind: a.Kind, UploadID: handle})
		}
		s, err := st.SendMessage(ctx, m)
		if err != nil {
			t.Fatalf("send %q in %s: %v", text, conv, err)
		}
		return s
	}
	read := func(conv uuid.UUID, key string, upTo int64) {
		if _, err := st.MarkRead(ctx, conv, id(key), upTo); err != nil {
			t.Fatalf("MarkRead(%s, %d): %v", key, upTo, err)
		}
	}
	voice := func(seed, ms int64, transcript string, segments ...[2]any) store.AttachmentRow {
		state := string(wire.TranscriptStatePending)
		doc := map[string]any{"eta_sec": 20}
		if transcript != "" {
			state = string(wire.TranscriptStateReady)
			segs := make([]map[string]any, len(segments))
			for i, s := range segments {
				segs[i] = map[string]any{"at_ms": s[0], "text": s[1]}
			}
			doc = map[string]any{"text": transcript, "segments": segs, "engine": "whisper-l3", "language": "en"}
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return store.AttachmentRow{
			Kind: "voice", StorageKey: fmt.Sprintf("voice/canvas/%d.opus", seed),
			DurationMs: &ms, Peaks: canvasPeaks(seed, 96), TranscriptState: &state, TranscriptJSON: raw,
		}
	}
	image := func(filename string, w, h, bytes int64) store.AttachmentRow {
		return store.AttachmentRow{
			Kind: "image", StorageKey: "image/canvas/" + filename,
			Filename: &filename, Width: &w, Height: &h, Bytes: &bytes,
		}
	}

	// The rooms and directs. Everything is sent in rail order from the bottom
	// up, so the rail's recency puts Kitchen Table on top — the conversation
	// the canvas, and so the app, opens on.
	bergen := room("Bergen Hill Co-op", "ilse", "nadia", "marek", "ted", "rosa", "petra", "wren")
	sunday := room("Sunday Dinner", "ilse", "nadia", "rosa", "ted")
	shed := room("Shed Projects", "marek", "ted")
	kitchen := room("Kitchen Table", "ilse", "nadia", "marek", "ted", "rosa", "wren")

	// ── the directs ─────────────────────────────────────────────────────────
	petraDM := direct("petra")
	say(petraDM, "petra", "sent the last invoice through Ilse, easier that way", uuid.Nil)
	read(petraDM, "hollis", 1)

	oskarDM := direct("oskar")
	say(oskarDM, "hollis", "still spelunking through the archive for that old permit", uuid.Nil)
	say(oskarDM, "hollis", "", uuid.Nil, voice(5501, 14_000, ""))

	wrenDM := direct("wren")
	say(wrenDM, "hollis", "the kombucha starter is ready whenever you want a jar", uuid.Nil)
	say(wrenDM, "hollis", "", uuid.Nil, voice(5502, 9_000, ""))

	rosaDM := direct("rosa")
	say(rosaDM, "rosa", "that’s the one, thank you", uuid.Nil)
	read(rosaDM, "hollis", 1)

	// Ted's direct holds nothing yet: its row on the canvas is a FAILED send,
	// which is an outbox entry the bundle provokes from this server itself.
	direct("ted")

	nadiaDM := direct("nadia")
	say(nadiaDM, "nadia", "", uuid.Nil, voice(1777, 107_000,
		"I can juggle the pickup either way, just tell me which day. If it’s Thursday I can take the truck, otherwise somebody else has to, because I’ve got the closing shift and no rush on any of it, truly.",
		[2]any{0, "I can juggle the pickup either way, just tell me which day."},
		[2]any{66_000, "If it’s Thursday I can take the truck, otherwise somebody else has to,"},
		[2]any{88_000, "because I’ve got the closing shift and no rush on any of it, truly."}))
	say(nadiaDM, "nadia", "no rush on any of it, truly", uuid.Nil)
	read(nadiaDM, "hollis", 2)

	marekDM := direct("marek")
	say(marekDM, "hollis", "sent it to your inbox instead", uuid.Nil)

	// Unread, with a transcript still pending.
	ilseDM := direct("ilse")
	say(ilseDM, "ilse", "", uuid.Nil, voice(4477, 72_000, ""))

	// ── the rooms below Kitchen Table ───────────────────────────────────────
	say(shed, "marek", "", uuid.Nil, image("shed_wall.jpg", 2016, 3024, 3_355_443))
	read(shed, "hollis", 1)
	if _, err := r.pool.Exec(ctx,
		`UPDATE conversation_members SET muted = true WHERE conversation_id = $1 AND user_id = $2`, shed, me); err != nil {
		t.Fatalf("mute Shed Projects: %v", err)
	}

	say(sunday, "rosa", "every Thursday since March, same table, same time", uuid.Nil)
	say(sunday, "hollis", "bringing the big pot", uuid.Nil)
	read(sunday, "hollis", 2)
	for _, k := range []string{"ilse", "nadia", "rosa", "ted"} {
		read(sunday, k, 2)
	}

	// Petra's old message stays hers after she is offboarded; the two after
	// it are unread.
	say(bergen, "petra", "handed the spare gate key to Ted before rotation ends — deliveries can go through him from here", uuid.Nil)
	say(bergen, "ted", "pallet is confirmed for the 2 to 6 window, depot said they’ll call ahead", uuid.Nil)
	say(bergen, "ted", "the delivery window moved to Thursday afternoon, I’ll confirm with the depot in the morning", uuid.Nil)
	read(bergen, "hollis", 1)

	// ── Kitchen Table, as the canvas draws it ───────────────────────────────
	say(kitchen, "nadia", "Anyone know whether the co-op still does the Thursday pickup, or did that move for the summer? I’ve got a standing order I’d rather not lose.", uuid.Nil)
	window := say(kitchen, "hollis", "Still Thursday. They moved the window, not the day — 2 to 6 now instead of noon.", uuid.Nil)
	ilseVoice := say(kitchen, "ilse", "", uuid.Nil, voice(9931, 38_000, ilseTranscript,
		[2]any{0, "Okay so I talked to Ted about the delivery and"},
		[2]any{6_000, "the short version is Thursday still works but they want"},
		[2]any{13_000, "everything staged by two, which means somebody has to be"},
		[2]any{20_000, "at the shed in the morning to sign for the pallet."},
		[2]any{27_000, "If nobody can do that I’ll ask them to hold it until Friday,"},
		[2]any{33_000, "but then we’re into the weekend and I’d rather not."},
		[2]any{38_000, "Also the tension gauge came back from the shop, it’s in the blue case on the bench."}))
	say(kitchen, "ilse", "photo from this morning, the whole run is re-tensioned", uuid.Nil)
	photo := say(kitchen, "ilse", "", uuid.Nil, image("IMG_4471.HEIC", 3024, 2016, 2_202_009))
	eight := say(kitchen, "hollis", "I can be there at eight to sign for it.", uuid.Nil)
	coffee := say(kitchen, "hollis", "bringing coffee for whoever else shows up", uuid.Nil)
	depot := say(kitchen, "nadia", "depot hours are here if anyone needs them — riverline.org/depot-hours", uuid.Nil)
	// Pending: playable at once, not searchable yet, and the reply stub to it
	// says so.
	marekVoice := say(kitchen, "marek", "", uuid.Nil, voice(6151, 52_000, ""))
	say(kitchen, "nadia", "Perfect, that’s the one I needed. Standing order is safe then.", window.ID)
	staging := say(kitchen, "hollis", "Staging by two is fine. I’ll be at the shed from eight.", ilseVoice.ID)
	say(kitchen, "marek", "That span looks straighter than it did in March.", photo.ID)
	say(kitchen, "ted", "This is out of date — they haven’t updated it since the window moved.", depot.ID)
	say(kitchen, "rosa", "Listened to it, I’ll take Friday morning.", marekVoice.ID)

	// The receipts behind the canvas's counts, which are high-water marks: the
	// reader's own mark sits on "Still Thursday", so everything after it is
	// the "N NEW" run; everyone has passed that message (READ 7/7); three
	// people have passed "at eight" (READ 4/7, the author counted); one has
	// passed the coffee (READ 2/7); and nobody has reached "Staging by two",
	// which is therefore SENT. Sending does not move a sender's own mark.
	read(kitchen, "hollis", window.Seq)
	read(kitchen, "ilse", coffee.Seq)
	read(kitchen, "nadia", eight.Seq)
	read(kitchen, "ted", eight.Seq)
	for _, k := range []string{"marek", "rosa", "wren"} {
		read(kitchen, k, window.Seq)
	}
	if staging.Seq <= coffee.Seq {
		t.Fatalf("seed order: staging (%d) must follow the coffee (%d)", staging.Seq, coffee.Seq)
	}

	// Offboarded after everything above: their messages keep their author,
	// and the directs with them are marked (CANT-138).
	for _, k := range []string{"petra", "oskar"} {
		if _, err := st.DeactivateUser(ctx, id(k)); err != nil {
			t.Fatalf("DeactivateUser(%s): %v", k, err)
		}
	}
	if len(uploads) != 0 {
		t.Fatalf("%d prepared attachment(s) were never sent", len(uploads))
	}
	return canvas{
		hollisToken: hollisToken, nadia: id("nadia"), kitchen: kitchen,
		hollis: me, marek: id("marek"), shed: shed,
	}
}
