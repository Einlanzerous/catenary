# CANT-47 — media storage: local disk behind a `MediaStore` seam, stable signed URLs

> **The Switchyard plan is the decision of record.** CANT-47 is Mode B and gated on one: **plan rev 3, approved 2026-10-23**, with all five rulings picked by a person, each on the recommended option. This file is a derived stub. It carries **outcomes and one-line reasons, not the argument**, because the argument is what drifts — two documents making the same case is the CHRN-79 shape, and the second copy is the one that goes stale.

Built by **CANT-48** (the server half: store, upload, ingest, serving), **CANT-49** (the web client's inline image and lightbox), **CANT-247** (the clients' `Uploader`, picker and recorder), **CANT-50** (backup), with **CANT-307** (blob garbage collection, a Mode C sub-task of CANT-67), **SERV-244** (the deploy change in `construct-server`) and **CANT-308** (link previews, its own decision) filed alongside.

## What was picked

| ruling | outcome | why, in one line |
|---|---|---|
| where media bytes live | **Local disk on this box, behind a `MediaStore` interface**; R2 and Trestle stay drop-in implementations of it | nothing yet needs an off-box store, and no R2 credential exists to build against |
| how bytes get in | **One authenticated request through the binary**: `POST /uploads?kind=image\|voice`, the bearer credential, the raw body, a single-use handle back | presigning buys nothing while the bytes land on this box anyway |
| how a media `url` is made | **A stable HMAC-signed URL with no expiry**: `/media/{key}?kid=&sig=`, a pure function of the key | journaled records never go stale, and `<img>` and `<audio>` need no header |
| how a thumbnail reaches the client | **A variant of the same URL**, `&v=thumb`; the wire schema is unchanged | no schema bump, no regenerated vectors |
| where blob garbage collection lives | **A sub-task of CANT-67, filed `review_mode: full`** | it deletes authored content, which is what Mode C is generated from |

## 1 · The store

```go
type MediaStore interface {
    Put(ctx context.Context, r io.Reader, max int64) (key string, bytes int64, err error) // streams, hashes, commits; idempotent on the hash
    Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)              // ErrNotFound
    Delete(ctx context.Context, key string) error                                         // idempotent
}
```

The first implementation is local disk under `CATENARY_MEDIA_DIR`, laid out as `blobs/<hh>/<sha256>` with temp-file-then-rename writes and a `tmp/` swept at boot. **That is Trestle's layout** (`docs/decisions/serv-134-trestle-v1.md` in its repository), so a later move into Trestle or R2 is a copy of a directory tree. The key is the SHA-256 of the **stored** bytes, after ingest. Names, kinds and MIME types live in Postgres, never in a path. Voice audio is stored **as recorded** (Invariant 4); its peaks and the Opus encode are CANT-56's.

### Trestle as the later home for private media

Not built, and not ticketed. If it becomes the store, these are its gaps, from its design of record, and they are the requirements:

1. `/m/` is public by its invariant 3, so private blobs need a separate serve surface with signed or per-request access.
2. Its API is loopback-only and proxies bytes under a bearer token, so a phone or browser cannot reach it.
3. Content-addressed dedup with per-token ownership is a privacy side channel for private data, and delete has to be reference-aware.
4. Quota and ownership are per token name, not per person.
5. EXIF stripping, thumbnails and dimensions are on its "Out" list.
6. Its index is one JSON file per blob held in memory, sized for thousands of blobs, not a photo library.

## 2 · Upload

`POST /uploads?kind=image|voice`: the handler streams through `MediaStore.Put` under a per-kind byte cap, runs ingest (images: sniff the real type, decode, apply EXIF orientation to the pixels, re-encode without metadata, record dimensions, make the thumbnail and the blurhash; voice: store as recorded), inserts one `uploads` row and answers `201 {"upload_id"}`. The handle is single-use, bound to the uploader (CANT-85 contract line 4) and expires if unconsumed. The client's `Content-Type` is never trusted. Over the cap is `413`, an unsupported type `415`. HEIC and AVIF cannot be decoded in a `CGO_ENABLED=0` binary and are refused with a typed error; CANT-48 checks with the Pixel before fixing the supported list. Proposed caps, settled in CANT-48: `CATENARY_MAX_IMAGE_BYTES` 16 MiB, `CATENARY_MAX_VOICE_BYTES` 8 MiB.

**CANT-48's `Done when` changes** from "presigned and direct" to "authenticated and streamed", and gains that its migration applies and its tests pass against a real database. Two description strings in the wire schema become wrong and are reworded **in CANT-48's PR**, not here, because rewording them moves the generated TypeScript, Dart and Go and the staleness guard:

- the upload handle's description, "Handle returned by the presigned-upload flow", in `schema/catenary.wire.v1.schema.json` (and its `openapi.yaml` twin), becomes "Handle returned by the upload endpoint";
- `VoiceAttachment.url`'s "may be presigned and time-limited", at the same two places, becomes "Opaque to the client; stable, not time-limited".

## 3 · Serving

`GET /media/{key}?kid=<n>&sig=<base64url>`, `sig = HMAC-SHA256(secret[kid], "catenary-media/v1\n" + key)`. No expiry and no viewer in the signature. Secrets are `CATENARY_MEDIA_URL_KEYS=1=<hex>,2=<hex>`: the last entry signs, all verify, and boot refuses a missing or short key naming the variable. Headers: the stored (sniffed) type, `X-Content-Type-Options: nosniff`, `Cache-Control: private, max-age=31536000, immutable`, range support so audio can seek, `attachment` for anything not an image or audio type, no SVG at all. The existing CSP (`img-src 'self' data:`) already admits a same-origin `/media/`.

**`mediaURL(storageKey string) string` keeps its signature**: it takes no viewer and no clock, so `/sync`, the hub and REST still derive byte-identical URLs from one deriver (CANT-84). Nothing in the golden vectors depends on a `url` value:

```
$ grep -c '"url"' schema/vectors/vectors.json
9
$ grep -o '"url": *"https://[^/]*' schema/vectors/vectors.json | sort | uniq -c
      9 "url": "https://media.catenary.invalid
$ grep -rIn "catenary.invalid" . --exclude=vectors.json --exclude-dir=node_modules --exclude-dir=.git
internal/wireview/vectors_test.go:109-111     three of the same literals, as constants the Go test feeds its deriver
schema/catenary.wire.v1.schema.json:3         the schema $id, not a media URL
```

All nine are fixed literals on a reserved `.invalid` host, and the only reference outside the vector file is the Go test's own constants. No runner parses or builds a URL, so the nine are round-tripped as opaque strings, and changing what production derives changes no vector. Measured on main at `9c341c1`.

**The exposure, as accepted:** anyone holding a URL can fetch that one blob, with no account, for as long as the blob exists. Revocation is deleting the blob or retiring a `kid`, which breaks every older journaled URL until clients re-fetch, and a member deactivated through Purser keeps the URLs they already saw. That is the trust boundary the messages already live in (D1).

### Thumbnails

`&v=thumb` serves the thumbnail. `attachments` has no thumbnail column and is not changed, so the handler resolves it through `uploads`: `storage_key` to `thumb_key`. Consequently **consumed `uploads` rows are kept after a send**, and garbage collection counts a live row's `thumb_key` as a reference. A `v=thumb` request with no such row is `404`; it never falls back to the full image.

## 4 · Schema

`0013_uploads` (0012 is the highest on main):

```sql
CREATE TABLE uploads (
  id          uuid PRIMARY KEY,
  uploader    uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  kind        text NOT NULL CHECK (kind IN ('voice','image')),
  storage_key text NOT NULL,
  thumb_key   text,                -- image only; same store, own hash
  bytes       bigint NOT NULL,
  width int, height int, placeholder text, filename text,   -- image
  created_at  timestamptz NOT NULL DEFAULT now(),
  consumed_at timestamptz         -- NULL until a send takes it
);
CREATE INDEX uploads_unconsumed_idx ON uploads (created_at) WHERE consumed_at IS NULL;
CREATE INDEX uploads_storage_key_idx ON uploads (storage_key);
```

This is the sketch CANT-48 is held to; the migration is the authority for the final columns. `users (id) ON DELETE RESTRICT` is how `0007_tokens` references accounts. The resolver is the CANT-85 contract: one `UPDATE … SET consumed_at … WHERE id = $1 AND uploader = $2 AND consumed_at IS NULL RETURNING …` per handle, inside the send's transaction, locking only this table, wired in `cmd/catenary/main.go` with `WithUploadResolver` (the default refuses every attachment, and forgetting the wire-up fails loudly in production rather than at compile time).

## 5 · Garbage collection

CANT-307, a sub-task of CANT-67, `review_mode: full`. Its predicate is bounded and part of what is read:

- an `uploads` row is **live** when unconsumed, or consumed with its `storage_key` still in `attachments`;
- a blob key is **referenced** when it is some `attachments.storage_key` or the `storage_key` or `thumb_key` of a live row;
- it deletes an unreferenced blob only past a **grace period** (24 hours proposed), in **capped batches** (500 proposed), **driven from the database** and never from a directory listing, the **file before its row**, and **dry-run first**.

`attachments.message_id` is `ON DELETE CASCADE`, so deleting a message leaves its bytes; nothing can orphan a blob until CANT-63 or CANT-67 can delete a message, so this is built last.

## 6 · Backup

The media volume joins `construct-backup`, the estate's restic job (a hub repository and a desktop repository; CANT-68 already uses it for `catenary_drill`). **The database is backed up before the media**: blobs are only ever added between the two snapshots, so a row can never point at a blob the media snapshot lacks, while backing up in the other order can produce a restored message with a dead image. CANT-50's one-file restore is checked by `sha256sum` of the restored file against its own key, with no database needed.

**Not verified:** `construct-backup`'s configuration, and whether its sources are ordered, live in `construct-server` and could not be read from here. That is the first step of SERV-244 and CANT-50.

## Stated here, built elsewhere

- **SERV-244** adds the `catenary_media` volume (writable by the image's `nobody` user), `CATENARY_MEDIA_DIR` and `CATENARY_MEDIA_URL_KEYS` (a Signet secret, never in a file or ticket) and the backup include. Needed before a **promote** of the upload code, not before CANT-48 merges. `deploy/Dockerfile`'s comment that the process "writes nothing to disk" is corrected in CANT-48's PR, and `deploy/README.md` gains the decision.
- **CANT-308** (link previews) is its own `decision` ticket: a server-side fetch of a URL a user typed, on a box beside Postgres and Purser, is an SSRF and privacy question this decision does not take. The Switchyard-side ticket magos asked for is a separate act in the SWY project.
- **CANT-247** is blocked by CANT-48. A **voice** attachment also needs `peaks` (the wire requires it), computed server-side in CANT-56, so voice cannot be sent for real until CANT-56 lands; images can ship first. The two are linked.
- **CANT-49** was linked only to CANT-47; it now follows CANT-48 as well.

## What changes in the other tickets

| ticket | change |
|---|---|
| CANT-48 | `Done when`: authenticated and streamed, not presigned; migration applies and tests pass against a real database; the two schema description rewordings and their regeneration are in its PR |
| CANT-49 | follows CANT-48; the inline row asks for `&v=thumb`, the lightbox for the plain URL |
| CANT-50 | the path is a restic include of the media volume, after the database; the restore is one file, verified by `sha256sum` against its key; blocked by SERV-244 |
| CANT-247 | the `Uploader` is one authenticated `POST /uploads`; blocked by CANT-48, related to CANT-56 for voice |
