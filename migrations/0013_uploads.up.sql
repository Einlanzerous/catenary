-- CANT-309 (CANT-48 sub-task A) · 0013_uploads — the pending-and-consumed upload
-- handles CANT-47 decided: one authenticated POST /uploads writes a blob and one
-- row here, and the send that names the handle consumes the row.
--
-- ADDITIVE ONLY. A new table touches no existing row, and nothing references it
-- yet: attachments.storage_key stays an opaque text column, so a message sent
-- before this migration is untouched. The down migration drops the table and so
-- forgets which blobs are pending or which thumbnail belongs to an image; the
-- blobs themselves are files and are not touched.

CREATE TABLE uploads (
    id          UUID PRIMARY KEY,

    -- RESTRICT, as 0007_tokens references accounts: an account with uploads is
    -- deactivated, never deleted out from under the handle that names it.
    uploader    UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,

    kind        TEXT        NOT NULL CHECK (kind IN ('voice', 'image')),

    -- The SHA-256 of the STORED bytes (CANT-47 section 1), which is also the blob's
    -- name under CATENARY_MEDIA_DIR. Names, kinds and types live here, never in
    -- a path.
    storage_key TEXT        NOT NULL,

    -- image only. The thumbnail is its own blob with its own hash, resolved
    -- through this row because attachments has no thumbnail column.
    thumb_key   TEXT,

    bytes       BIGINT      NOT NULL CHECK (bytes >= 0),

    -- image
    width       INTEGER     CHECK (width >= 1),
    height      INTEGER     CHECK (height >= 1),
    placeholder TEXT,
    filename    TEXT,

    -- voice. NULL until CANT-56 computes them server-side: attachments requires
    -- both on a voice row (attachments_voice_fields), so a voice upload with
    -- neither cannot be sent and the resolver refuses it without consuming it.
    duration_ms INTEGER     CHECK (duration_ms >= 0),
    peaks       SMALLINT[]  CHECK (
                    array_length(peaks, 1) <= 512
                    AND 0 <= ALL (peaks)
                    AND 100 >= ALL (peaks)
                ),

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- NULL until a send takes the handle. The row is KEPT after that, because
    -- the thumbnail lookup and blob garbage collection both read it.
    consumed_at TIMESTAMPTZ
);

CREATE INDEX uploads_unconsumed_idx ON uploads (created_at) WHERE consumed_at IS NULL;
CREATE INDEX uploads_storage_key_idx ON uploads (storage_key);

COMMENT ON TABLE uploads IS
    'CANT-47 / CANT-309. One row per uploaded blob; id is the single-use handle a send names. consumed_at is set inside the send''s transaction by the resolver''s row-locked UPDATE (CANT-85 contract). Consumed rows are kept: serving a thumbnail and blob garbage collection read them.';
COMMENT ON COLUMN uploads.peaks IS
    'Waveform bars, 0-100, at most 512, voice only. Computed server-side (CANT-56); NULL means the voice upload is not yet sendable.';
