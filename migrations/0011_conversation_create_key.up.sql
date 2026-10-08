-- CANT-253 ruling 2 / CANT-266 · 0011_conversation_create_key — the idempotency
-- key a retried POST /conversations is recognised by.
--
-- ADDITIVE ONLY: a nullable column add rewrites nothing and takes only a brief
-- ACCESS EXCLUSIVE lock on a table with one row per conversation of a small
-- group. Every conversation that exists today starts with NULL, which is also
-- the right value for a direct (find-or-create by direct_key) and for any group
-- made without a request_id.
ALTER TABLE conversations ADD COLUMN create_key TEXT;

COMMENT ON COLUMN conversations.create_key IS
    'CANT-253 ruling 2. "<creator id>|<request id>" for a group made with a request_id, NULL otherwise. Stored rather than derived: it records an idempotency fact the server cannot recompute from any other column. Never on the wire as its own field; CreateGroupRequest.request_id (CANT-265) is mapped as derived for that reason. Mirrors direct_key''s idiom: INSERT ... ON CONFLICT DO NOTHING, then read the winner back inside the same transaction.';

-- Partial, so conversations without a key never contend for the index: this is
-- a uniqueness rule, not a presence rule. The creator's id is inside the key,
-- so the same request_id from a different creator is a different key.
CREATE UNIQUE INDEX conversations_create_key_idx ON conversations (create_key) WHERE create_key IS NOT NULL;

COMMENT ON INDEX conversations_create_key_idx IS
    'CANT-253 ruling 2. What makes two concurrent creates with one request_id yield one row: the loser''s ON CONFLICT DO NOTHING inserts nothing and reads the winner back.';
