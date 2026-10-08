-- CANT-254 ruling 0 (B) / CANT-256 · 0012_self_conversation — a conversation with
-- only yourself.
--
-- ADDITIVE ONLY. Widening a CHECK and adding a partial index touch no row:
-- every conversation that exists today is a 'direct' or a 'group', and both stay
-- valid. Nothing here creates a self conversation; FindOrCreateSelf (CANT-257)
-- does, one per person.
--
-- THE DOWN MIGRATION DELETES AUTHORED MESSAGES (see 0012_self_conversation.down.sql),
-- which is why this ticket is review_mode: full. A person who does not want that
-- rolls forward instead of down.

-- Postgres names an inline column CHECK <table>_<column>_check, and 0002 wrote
-- this one inline. A CHECK cannot be altered, only replaced.
ALTER TABLE conversations DROP CONSTRAINT conversations_kind_check;
ALTER TABLE conversations
    ADD CONSTRAINT conversations_kind_check CHECK (kind IN ('direct', 'group', 'self'));

-- The key of a self conversation is its owner's user id, so a person has at most
-- one. direct_key is reused rather than a column added: a self conversation is a
-- direct with one member, and the column is the "who is this conversation for"
-- lookup that find-or-create races on. Partial, so a direct's key (two ids joined)
-- and a self's key (one id) never contend for one index, and a group, whose
-- direct_key is NULL, is outside both.
CREATE UNIQUE INDEX conversations_self_key_idx
    ON conversations (direct_key) WHERE kind = 'self';

COMMENT ON INDEX conversations_self_key_idx IS
    'CANT-254. What makes two concurrent FindOrCreateSelf calls for one person yield one row: the loser''s ON CONFLICT DO NOTHING inserts nothing and reads the winner back. direct_key holds the owner''s user id for kind = ''self''.';
