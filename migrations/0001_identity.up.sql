-- CANT-13 · 0001_identity — who can speak, and from what.
--
-- Neither table deletes. `users.deactivated_at` is the offboard path and
-- `devices.revoked_at` is revocation, because `messages.author_id` and
-- `messages.sender_device_id` are RESTRICT: a row that anything ever authored
-- from cannot go away without taking authored messages with it.

CREATE TABLE users (
    id             UUID PRIMARY KEY,
    handle         TEXT        NOT NULL UNIQUE,
    display_name   TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The offboard path. Purser deactivates; it never deletes an author.
    -- Ruling 4: this is what keeps CANT-33 a Mode A ticket.
    deactivated_at TIMESTAMPTZ
);

COMMENT ON COLUMN users.display_name IS
    'Served live via SyncResponse.users and never frozen into a message, so a rename propagates to history instead of leaving the old name in every row.';
COMMENT ON COLUMN users.deactivated_at IS
    'Purser offboard. NULL = active. Deactivation rather than deletion because messages.author_id is ON DELETE RESTRICT — CANT-13 Ruling 4.';

CREATE TABLE devices (
    id           UUID PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,

    -- A revocation list is unusable if the rows do not say which phone they are.
    name         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ,

    -- CANT-30. Revocation is a column, not a delete, for the same reason
    -- deactivation is: messages.sender_device_id is RESTRICT.
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX devices_user_id_idx ON devices (user_id);

COMMENT ON COLUMN devices.revoked_at IS
    'CANT-30 revocation. NULL = live. A device row is never deleted; messages.sender_device_id is ON DELETE RESTRICT.';
