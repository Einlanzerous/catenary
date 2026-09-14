package main

// CANT-85's Done-when, third clause: the mapper serves the rows the send path
// writes, with no change to CANT-84.
//
// The oracle is the mapper itself, applied to what the resolver handed the
// store. If the column list position 11b writes and the column list Sync reads
// ever disagree — a column forgotten on either side, a type that does not
// round-trip — the served attachments stop matching the mapping of the rows the
// send was given, and this goes red. It is the only test that can see that,
// because it is the only one that goes all the way from SendMessage to the wire.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wireview"
)

// fixedResolver returns the same rows for every send, with their positions
// deliberately wrong, so the store's positioning is part of what is served.
type fixedResolver []store.AttachmentRow

func (f fixedResolver) Resolve(context.Context, pgx.Tx, uuid.UUID, []store.NewAttachment) ([]store.AttachmentRow, error) {
	out := append([]store.AttachmentRow(nil), f...)
	for i := range out {
		out[i].Position = len(out) - 1 - i
	}
	return out, nil
}

func TestTheMapperServesTheAttachmentRowsASendWrites(t *testing.T) {
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := store.MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Maximal rows: every column each kind can carry, set to something
	// distinguishable, so a column lost anywhere between the insert and the wire
	// is a field missing from the comparison.
	durationMs, width, height, bytes := int64(8421), int64(3024), int64(4032), int64(1874211)
	ready, filename, placeholder := "ready", "harbour.heic", "LKO2?U%2Tw=w]~RBVZRi};RPxuwH"
	resolved := fixedResolver{
		{
			Kind:            "voice",
			StorageKey:      "voice/2026/09/a1b2c3.m4a",
			DurationMs:      &durationMs,
			Peaks:           []int64{0, 3, 17, 42, 88, 100, 64, 9},
			TranscriptState: &ready,
			TranscriptJSON: []byte(`{"text":"back by six, bring the ladder","word_count":6,` +
				`"segments":[{"at_ms":0,"text":"back by six,"},{"at_ms":1400,"text":"bring the ladder"}],` +
				`"engine":"whisper.cpp small.en","language":"en","eta_sec":2}`),
		},
		{
			Kind:        "image",
			StorageKey:  "image/2026/09/d4e5f6.heic",
			Filename:    &filename,
			Width:       &width,
			Height:      &height,
			Bytes:       &bytes,
			Placeholder: &placeholder,
		},
	}
	st := store.New(pool, store.DefaultLimits(), slog.New(slog.DiscardHandler)).WithUploadResolver(resolved)

	author := mkUser(ctx, t, pool, "ana", "Ana")
	conv := mkGroup(ctx, t, pool, "Harbour", author)
	text := "photo and a note"
	sent, err := st.SendMessage(ctx, store.NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: &text,
		Attachments: []store.NewAttachment{
			{Kind: "voice", UploadID: uuid.New()},
			{Kind: "image", UploadID: uuid.New()},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	page, err := serveSync(ctx, st, author, 0, 0)
	if err != nil {
		t.Fatalf("serveSync: %v", err)
	}

	// What the mapper makes of the rows the resolver handed the store, in the
	// send's order — the same serveSync viewer, so url derivation is identical.
	expected := append([]store.AttachmentRow(nil), resolved...)
	for i := range expected {
		expected[i].Position = i
	}
	mapped := wireview.Message(store.MessageRow{ID: sent.ID, ConversationID: conv, AuthorID: author, At: sent.At},
		expected, nil, wireview.Viewer{UserID: author, MediaURL: func(k string) string { return k }})
	want, err := json.Marshal(mapped.Attachments)
	if err != nil {
		t.Fatalf("encode expected: %v", err)
	}

	var found bool
	for _, m := range page.Messages {
		if string(m.ID) != sent.ID.String() {
			continue
		}
		found = true
		if len(m.Attachments) != len(resolved) {
			t.Fatalf("served %d attachments, want %d", len(m.Attachments), len(resolved))
		}
		got, err := json.Marshal(m.Attachments)
		if err != nil {
			t.Fatalf("encode served: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("the served attachments are not the mapping of the rows the send was given:\n got  %s\n want %s", got, want)
		}
	}
	if !found {
		t.Fatalf("the sent message (%s) is not on the page; nothing was compared", sent.ID)
	}
}
