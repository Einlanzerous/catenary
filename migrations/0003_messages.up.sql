-- CANT-13 · 0003_messages — the append-only log, and the counter that orders it.
--
-- This is the table that only ever grows and the one nothing else can be
-- locked against, so every column here is a column that would cost a backfill
-- to add later.

CREATE TABLE messages (
    id              UUID PRIMARY KEY,
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE RESTRICT,

    -- RESTRICT, and this is the sentence that matters: it is what keeps
    -- CANT-33 off the Mode C list. A Purser offboard cannot destroy authored
    -- messages, because it cannot delete a user who has authored any — it sets
    -- users.deactivated_at instead.
    author_id       UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,

    -- DENSE within a conversation. If a client can see seq 4 and seq 6 it may
    -- assume seq 5 exists and it is missing it. That is why idempotency is
    -- checked BEFORE this is drawn: a replay that draws and rolls back is
    -- fine, but a replay that draws and COMMITS leaves a permanent hole the
    -- client reads as a lost message.
    seq             BIGINT      NOT NULL CHECK (seq >= 1),

    -- SERVER-GLOBAL and sparse. One counter for the whole deployment, which
    -- each account reads as a cursor into the subset it may see, so gaps carry
    -- no information. Not a bigserial: a sequence hands out its number outside
    -- the transaction, so two inserters can commit out of order and a client
    -- that has seen 100 never asks for 99 again.
    log_seq         BIGINT      NOT NULL UNIQUE,

    -- Equal to log_seq at insert. Landed NOW rather than by CANT-63 because a
    -- NOT NULL column with a per-row default is a full-table rewrite when
    -- added later — Postgres's metadata-only ADD COLUMN fast path covers
    -- constant defaults only. Against an empty table it costs nothing.
    -- CANT-63 decides whether it ever moves.
    updated_log_seq BIGINT      NOT NULL UNIQUE,

    -- SERVER-ASSIGNED. clock_timestamp() rather than now(): now() is the
    -- transaction's start time, so two concurrent inserts can take their
    -- timestamps in the opposite order to the row lock that ordered their
    -- seqs — and the wire promises timestamps sort chronologically as strings.
    -- clock_timestamp() is read after the lock, so `at` order and `seq` order
    -- agree.
    at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    text            TEXT,

    -- SET NULL. A reply is newer than its source by definition, so RESTRICT
    -- would make CANT-67's retention sweep fail every time it reached a source
    -- with a reply. The wire builds ReplyRef live and Message.reply_to is
    -- optional, so a swept source is already "no ref".
    reply_to        UUID        REFERENCES messages (id) ON DELETE SET NULL,

    -- The idempotency key. Scoped (author_id, client_id) to match
    -- ClientSend.client_id's normative "the server deduplicates on
    -- (account, client_id)". NOT (sender_device_id, client_id): that column is
    -- nullable for bots, Postgres treats NULLs as distinct in a unique
    -- constraint, and a device-less sender would get no deduplication at all.
    client_id       UUID,

    -- Nullable: a bot has no device. RESTRICT, paired with devices.revoked_at.
    sender_device_id UUID       REFERENCES devices (id) ON DELETE RESTRICT,

    edited_at       TIMESTAMPTZ,
    deleted         BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Search denormalisation. The single to_tsvector index over text and
    -- transcripts is CANT-59's; the column lands here so that ticket is an
    -- index and not a backfill against this table.
    transcript_text TEXT,

    UNIQUE (conversation_id, seq),
    UNIQUE (author_id, client_id)
);

-- /sync?after= reads log_seq order. The UNIQUE above already provides the
-- index; named here so the intent is not mistaken for incidental.
CREATE INDEX messages_conversation_id_idx ON messages (conversation_id);
-- Without this, ON DELETE SET NULL scans the table for every swept message.
CREATE INDEX messages_reply_to_idx ON messages (reply_to) WHERE reply_to IS NOT NULL;
CREATE INDEX messages_sender_device_id_idx ON messages (sender_device_id) WHERE sender_device_id IS NOT NULL;

COMMENT ON COLUMN messages.seq IS
    'Per-conversation and DENSE: a gap means the client is missing a message. Drawn from conversations.last_seq inside the inserting transaction (CANT-14).';
COMMENT ON COLUMN messages.log_seq IS
    'SERVER-GLOBAL and sparse — ONE counter for the whole deployment, not one per account. Each account reads it as a cursor into the subset it may see, so its gaps carry no information. The only thing /sync?after= takes.';
COMMENT ON COLUMN messages.updated_log_seq IS
    'Equal to log_seq at insert. Present from the first migration so CANT-63 cannot be forced into a full-table rewrite of messages; whether it ever moves is CANT-63''s to decide.';
COMMENT ON COLUMN messages.at IS
    'Server-assigned at insert via clock_timestamp(), not client-supplied and not now(): client clocks and transaction-start times both put `at` order in disagreement with `seq` order.';
COMMENT ON COLUMN messages.client_id IS
    'Idempotency key, unique per (author_id, client_id) to match ClientSend.client_id''s normative (account, client_id). NULL for server-originated rows; Postgres treats those NULLs as distinct, which is correct — they were never deduplicated.';

CREATE TABLE attachments (
    id              UUID PRIMARY KEY,

    -- CASCADE: the row goes with the message. The BYTES are CANT-47's and
    -- CANT-67's problem, named here rather than left implied.
    message_id      UUID    NOT NULL REFERENCES messages (id) ON DELETE CASCADE,

    kind            TEXT    NOT NULL CHECK (kind IN ('voice', 'image')),

    -- OPAQUE, and deliberately not a URL. VoiceAttachment.url is documented on
    -- the wire as "may be presigned and time-limited": a presigned R2 URL is
    -- stale within minutes and a local path is a Traefik route, so the value
    -- is a function of CANT-47, which this migration defers. `url` is derived
    -- at serve time from this key.
    storage_key     TEXT    NOT NULL,

    position        INTEGER NOT NULL CHECK (position >= 0),

    duration_ms     INTEGER CHECK (duration_ms >= 0),

    -- Computed SERVER-SIDE: the seeded generator overflows 2^53 and produces
    -- different bars in JavaScript than in Dart. Bounds both the length and
    -- the ELEMENTS, because the wire bounds both.
    peaks           SMALLINT[] CHECK (
                        array_length(peaks, 1) <= 512
                        AND 0 <= ALL (peaks)
                        AND 100 >= ALL (peaks)
                    ),

    -- Transcript.state is REQUIRED on VoiceAttachment, so without it the
    -- server cannot serve a voice note at all — not even a pending one.
    transcript_state TEXT   CHECK (transcript_state IN ('pending', 'ready', 'failed')),
    -- word_count, segments[], engine, language, eta_sec. Authoritative here;
    -- messages.transcript_text is the search denormalisation of `text`.
    transcript_json JSONB,

    filename        TEXT,
    width           INTEGER CHECK (width >= 1),
    height          INTEGER CHECK (height >= 1),
    bytes           BIGINT  CHECK (bytes >= 0),
    -- The blurhash, computed server-side on ingest.
    placeholder     TEXT,

    UNIQUE (message_id, position),

    -- Ruling 6: one table, per-kind CHECKs enforcing each kind's required set.
    -- One table because the wire has one `attachments` array; the CHECKs
    -- because a column nullable for the other kind is not a licence to omit it.
    CONSTRAINT attachments_voice_fields CHECK (
        kind <> 'voice' OR (
            duration_ms IS NOT NULL
            AND peaks IS NOT NULL
            AND transcript_state IS NOT NULL
        )
    ),
    CONSTRAINT attachments_image_fields CHECK (
        kind <> 'image' OR (
            filename IS NOT NULL
            AND width IS NOT NULL
            AND height IS NOT NULL
            AND bytes IS NOT NULL
        )
    )
);

CREATE INDEX attachments_message_id_idx ON attachments (message_id);

COMMENT ON COLUMN attachments.storage_key IS
    'Opaque storage key, NOT a URL. The wire''s url is derived from this at serve time, because whether it is a presigned R2 GET or a local path is CANT-47''s decision and a stored URL would be wrong the moment that lands.';
COMMENT ON COLUMN attachments.peaks IS
    'Waveform bars, 0-100, at most 512. Computed SERVER-SIDE because the seeded generator overflows 2^53 and produces different bars in JavaScript than in Dart.';

-- ONE COUNTER FOR THE WHOLE DEPLOYMENT, not one per account. A per-account
-- counter would be dense, and sparsity is the entire division of labour
-- between log_seq and seq. `CHECK (id = 1)` is what makes "one row" enforced
-- rather than conventional.
CREATE TABLE log_counter (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    value BIGINT  NOT NULL DEFAULT 0 CHECK (value >= 0)
);

INSERT INTO log_counter (id, value) VALUES (1, 0);

COMMENT ON TABLE log_counter IS
    'The server-global log_seq counter. EXACTLY ONE ROW for the whole deployment — CHECK (id = 1) enforces it. Not one row per account: that would make log_seq dense per account, and every gap would then look like a missing message. Drawn by UPDATE ... RETURNING inside the inserting transaction (CANT-14), never a bigserial — a sequence hands out its number outside the transaction and lets two inserters commit out of order.';
