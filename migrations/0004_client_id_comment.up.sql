-- CANT-77 · the client_id column comment claimed something CANT-75 contradicts.
--
-- 0003 shipped this column commented "NULL for server-originated rows". That
-- described a caller which does not exist: every sending surface supplies an
-- idempotency key, bots included — CANT-75's REST send takes client_id in its
-- body and its Done-when requires a sender with no device row be deduplicated
-- exactly like one with. CANT-14's store.SendMessage refuses the zero value
-- outright, so nothing in the service can write NULL here.
--
-- A comment-only migration rather than an edit to 0003, because 0003 is
-- applied: editing it in place would leave every deployed database carrying
-- the sentence this corrects.

COMMENT ON COLUMN messages.client_id IS
    'Idempotency key, unique per (author_id, client_id) to match ClientSend.client_id''s normative (account, client_id). NOTHING WRITES NULL: every sending surface supplies a key — CANT-75''s REST send included — and store.SendMessage refuses the zero value (CANT-14). Nullable as deliberate headroom for a genuinely server-originated row should one ever exist; Postgres would treat such NULLs as distinct, which is correct, because they were never deduplicated.';
