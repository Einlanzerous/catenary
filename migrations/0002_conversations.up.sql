-- CANT-13 · 0002_conversations — D4: everything is a conversation.
--
-- No special-cased 1:1 entity. A DM is kind = 'direct' with two members; a room
-- is kind = 'group' with more. Adding a third person to a DM is a row insert
-- plus `kind` changing, not a migration.

CREATE TABLE conversations (
    id             UUID PRIMARY KEY,

    -- MUTABLE. A third member promotes a direct to a group, which writes
    -- `kind`, writes `name`, and nulls `direct_key`.
    kind           TEXT        NOT NULL CHECK (kind IN ('direct', 'group')),

    -- A DM has no name: the rail shows the OTHER member, which is per reader
    -- and cannot be stored in one column. A group must have one.
    --
    -- The predicate reads "direct conversations are exempt", which is the way
    -- round that matches the nullability. Written the other way round it would
    -- force a meaningless string into every DM.
    name           TEXT        CHECK (kind = 'direct' OR name IS NOT NULL),

    -- The two user ids sorted and joined. Makes a DM a lookup rather than a
    -- search over members, so find-or-create (CANT-75) cannot race two
    -- conversations into existence for one pair.
    --
    -- REQUIRED on a direct, and that CHECK is what makes the partial unique
    -- index below mean anything: a unique index does not constrain NULLs, so
    -- without it two directs with no key are both accepted and the pair has two
    -- threads, each with its own dense seq. This is the same NULL-distinctness
    -- hole Ruling 2 rejected for (sender_device_id, client_id), reached from the
    -- other side of the schema. Evaluated per statement, so the promotion below
    -- — which sets kind and nulls direct_key together — still passes.
    direct_key     TEXT        CHECK (kind <> 'direct' OR direct_key IS NOT NULL),

    -- The per-conversation ordinal source. Bumped by UPDATE ... RETURNING
    -- inside the inserting transaction, which is what makes assignment order
    -- and commit order the same order.
    --
    -- The wire's `head_seq` is SERVED from this and is not a second column:
    -- with the row lock the bump and the insert commit together, so the two
    -- would be equal at every instant — one number stored twice.
    last_seq       BIGINT      NOT NULL DEFAULT 0 CHECK (last_seq >= 0),

    -- Ruling 5. NULL means inherit the global infinite default, NOT zero days.
    -- CHECK mirrors the wire's `minimum: 1`, so "0 days" is not expressible in
    -- either place.
    retention_days INTEGER     CHECK (retention_days >= 1),

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial: only directs are unique per pair. Groups may share a name freely,
-- and a promoted conversation nulls direct_key on the way out.
CREATE UNIQUE INDEX conversations_direct_key_idx
    ON conversations (direct_key) WHERE kind = 'direct';

COMMENT ON COLUMN conversations.retention_days IS
    'CANT-13 Ruling 5. NULL = inherit the global infinite default, NOT zero days. CANT-67''s sweep must read NULL as "keep forever"; reading it as 0 deletes everything. CHECK (retention_days >= 1) mirrors the wire''s minimum: 1, so 0 is not expressible here either.';
COMMENT ON COLUMN conversations.last_seq IS
    'Per-conversation dense ordinal source, bumped in the inserting transaction (CANT-14). The wire''s head_seq is served from this rather than stored separately.';
COMMENT ON COLUMN conversations.direct_key IS
    'The two member user ids sorted and joined. Unique among directs. Nulled when a direct is promoted to a group.';

CREATE TABLE conversation_members (
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE RESTRICT,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- CANT-26's write path. An author's own send advances their own read_seq —
    -- you have read what you just sent — which makes first_unread_seq
    -- arithmetic (read_seq + 1, absent when read_seq = last_seq) rather than a
    -- scan that has to skip your own messages.
    read_seq        BIGINT      NOT NULL DEFAULT 0 CHECK (read_seq >= 0),

    muted           BOOLEAN     NOT NULL DEFAULT FALSE,

    PRIMARY KEY (conversation_id, user_id)
);

-- "Which conversations am I in" is the rail's query and the primary key's
-- leading column is the wrong one for it.
CREATE INDEX conversation_members_user_id_idx ON conversation_members (user_id);

COMMENT ON COLUMN conversation_members.read_seq IS
    'Highest seq this member has read. An author''s own send advances it, which is what makes first_unread_seq arithmetic rather than a scan (CANT-26).';
