-- CANT-28 · 0007_tokens — the four credential shapes, and the column that says
-- which senders are not people.
--
-- ADDITIVE ONLY. Nothing here rewrites an existing column; `users.kind` takes a
-- DEFAULT so the table is not rewritten either. `devices.revoked_at` already
-- exists from 0001 and needed no migration — what it needed was a writer, and
-- that is `store.RevokeDevice` rather than anything in this file.
--
-- EVERY TOKEN IS STORED AS A HASH AND NEVER IN THE CLEAR. That is also what
-- lets a log line name a token by its `id` without naming the secret, which is
-- why every table here has one.
--
-- SHA-256 AND NOT A PASSWORD KDF, DELIBERATELY. Argon2 or bcrypt would be the
-- right answer for something a person chose and wrong for these: a token is 32
-- bytes from a CSPRNG, so there is no dictionary and no candidate set to slow
-- an attacker down over — and ruling 0 makes verification an indexed point-read
-- on every authenticated request, which a salted KDF cannot be. The entropy is
-- the defence; the hash is only there so a database copy is not a key ring.

-- A bot has no device, and every credential below this line hangs off one.
-- CANT-73 builds the minting; this is the column that lets a row say what it is.
-- `User.kind` on the wire is CANT-76's and deliberately not here — it would be
-- the first new plain enum under CANT-74's unresolved compatibility policy.
ALTER TABLE users ADD COLUMN kind TEXT NOT NULL DEFAULT 'person'
    CHECK (kind IN ('person', 'bot'));

COMMENT ON COLUMN users.kind IS
    'CANT-28 ruling 4 / CANT-73. `person` or `bot`. A bot authenticates with a long-lived non-rotating access token and cannot enrol a device or refresh; it is scoped by membership alone, and grants append-as-self only — no edit and no delete, including of its own messages, which is what keeps CANT-73 off the Mode C list. Purser never creates one: R6 settled that Purser provisions people.';

-- The bootstrap credential. R6: at provisioning time the person has zero
-- devices, so the only credential that can exist is one string, and per-device
-- refresh tokens are minted here long after Purser is out of the picture.
CREATE TABLE enrolment_tokens (
    id                 UUID PRIMARY KEY,
    user_id            UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    token_hash         BYTEA       NOT NULL UNIQUE,
    issued_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL,

    -- SINGLE USE IS A COLUMN, NOT A DELETE, for the same reason deactivation
    -- and revocation are: a redemption that leaves no row cannot be audited
    -- afterwards, and "was this token ever used, and by what?" is the first
    -- question anyone asks about a credential that turns up somewhere odd.
    redeemed_at        TIMESTAMPTZ,
    redeemed_by_device UUID REFERENCES devices (id) ON DELETE RESTRICT,

    -- Provision RE-ISSUES on re-invite, which R6 classes as a rotation
    -- Provision may perform and Reconcile may not. The previous token is
    -- superseded rather than deleted, on the same argument as redeemed_at.
    superseded_at      TIMESTAMPTZ,

    -- Redeemed and superseded are mutually exclusive: a token that was used is
    -- not also a token that was withdrawn, and a row claiming both would make
    -- the audit trail unreadable in exactly the case somebody is reading it.
    CHECK (redeemed_at IS NULL OR superseded_at IS NULL),

    -- redeemed_by_device travels with redeemed_at or not at all. Either half
    -- alone is a redemption that cannot say what redeemed it, or a device
    -- attributed to a redemption that never happened.
    CHECK ((redeemed_at IS NULL) = (redeemed_by_device IS NULL))
);

-- EXACTLY ONE REDEEMABLE TOKEN PER PERSON, ENFORCED HERE RATHER THAN IN THE
-- RE-ISSUE QUERY. R6's re-invite case is the one Lyceum's connector gets wrong,
-- and it gets it wrong by leaving the old token live — a mistake that is
-- invisible until two enrolments succeed. A partial unique index turns
-- forgetting to supersede into a constraint violation at the moment it happens.
--
-- Expiry is NOT in the predicate. An expired unredeemed token still holds the
-- slot, so a re-issue must supersede it like any other; folding expiry in here
-- would let a second live row appear the instant the first one lapsed, which is
-- a second redeemable token by another name.
CREATE UNIQUE INDEX enrolment_tokens_one_live_per_user_idx
    ON enrolment_tokens (user_id)
    WHERE redeemed_at IS NULL AND superseded_at IS NULL;

-- One per device, rotating. CANT-29 owns what happens when one is replayed.
CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY,
    device_id   UUID        NOT NULL REFERENCES devices (id) ON DELETE RESTRICT,
    token_hash  BYTEA       NOT NULL UNIQUE,

    -- family_id AND replaced_by ARE CANT-29'S, AND THEY ARE HERE ANYWAY.
    -- Rotation writes them; reuse detection reads them. Adding them later means
    -- a full-table rewrite against rows that are live credentials on people's
    -- phones — the same argument `messages.updated_log_seq` records for
    -- CANT-63, and the reason this ticket lands the columns for a ticket that
    -- has not started.
    --
    -- A family is the chain from one enrolment: the first token's family_id is
    -- its own id, and every rotation carries it forward. So "invalidate the
    -- family" is one predicate over one indexed column rather than a walk.
    family_id   UUID        NOT NULL,
    replaced_by UUID REFERENCES refresh_tokens (id) ON DELETE RESTRICT,

    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX refresh_tokens_device_id_idx ON refresh_tokens (device_id);

-- NO INDEX ON family_id, and that is a decision rather than an omission. The
-- only query that would use it is CANT-29's family invalidation, which does not
-- exist yet, and 0006 already records the house position that a speculative
-- index is a thing nobody later dares remove. The COLUMN has to be here because
-- adding it later rewrites the table; the INDEX does not, so it belongs to the
-- ticket whose query needs it.

-- Short-lived for a person, long-lived for a bot. Ruling 0: opaque and looked
-- up, so revocation is a column write that takes effect on the next request
-- rather than at the next expiry.
CREATE TABLE access_tokens (
    id         UUID PRIMARY KEY,
    token_hash BYTEA       NOT NULL UNIQUE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,

    -- NULL means "not a device's token", which today means a bot's. The store
    -- additionally requires that such a row's user is `kind = 'bot'`; a CHECK
    -- cannot see another table, so that half is a store invariant with a test.
    device_id  UUID REFERENCES devices (id) ON DELETE RESTRICT,

    issued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- NULL MEANS NEVER EXPIRES, and the CHECK below is what stops that being a
    -- footgun. A bot token is long-lived and non-rotating because a cron job
    -- restarted from a snapshot cannot be expected to have rotated; a person's
    -- access token is fifteen minutes and must never be able to acquire a NULL
    -- here by accident. Tying the two together makes "no device, no expiry" the
    -- only shape either kind can take.
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,

    CHECK ((device_id IS NULL) = (expires_at IS NULL))
);

COMMENT ON TABLE access_tokens IS
    'CANT-28 ruling 0. Opaque random tokens, stored as a SHA-256 hash and verified by an indexed point-read on token_hash. Two shapes share the table: a person''s device-scoped token with an expiry, and a bot''s device-less token without one. Revocation is immediate because verification touches the row on every request.';
COMMENT ON TABLE enrolment_tokens IS
    'CANT-28. The bootstrap credential Purser issues, redeemable exactly once. Rows are never deleted: redeemed_at and superseded_at record what became of one, and the partial unique index keeps at most one redeemable per person — R6''s re-invite case.';
COMMENT ON TABLE refresh_tokens IS
    'CANT-28. One rotating credential per device. family_id and replaced_by are written by the rotation path and read by CANT-29''s reuse detection; they are present from this migration so that adding them later does not rewrite a table of live credentials.';
