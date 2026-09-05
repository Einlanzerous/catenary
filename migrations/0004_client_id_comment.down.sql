-- Restores 0003's wording verbatim. It is wrong (see the up migration), but a
-- down migration returns the schema to what the previous version shipped
-- rather than to what that version should have said.

COMMENT ON COLUMN messages.client_id IS
    'Idempotency key, unique per (author_id, client_id) to match ClientSend.client_id''s normative (account, client_id). NULL for server-originated rows; Postgres treats those NULLs as distinct, which is correct — they were never deduplicated.';
