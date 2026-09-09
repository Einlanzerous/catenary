-- CANT-89 — the change marker `/sync` pages on.
--
-- THREE COLUMNS, ONE COUNTER, ONE CURSOR. `/sync?after=` takes a single
-- `log_seq` and schema/FINDINGS.md §2.1 settled that a second cursor is not
-- available, so a marker has to be drawn from `log_counter` or it could not be
-- compared against the cursor at all. That is also why the trigger list is
-- short: everything that bumps one of these enters the deployment-wide
-- serialised section.
--
-- Which table answers which question:
--
--   conversations        shared metadata — name, kind, membership, retention.
--                        Changes rarely, by an explicit act, and concerns
--                        every member.
--   conversation_members per member — first_unread_seq and muted. Bumped by
--                        that member's own receipt, so a receipt wakes that
--                        member's other devices and nobody else's.
--   users                display_name. Bumped by a rename and by nothing else.
--
-- NOT UNIQUE, and that is load-bearing rather than incidental: these are
-- comparison values, not identities in the log — nothing addresses a row BY
-- one — and dropping the constraint is what lets the backfill below set every
-- conversation from a single draw instead of taking one per row.
ALTER TABLE conversations        ADD COLUMN metadata_log_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE users                ADD COLUMN metadata_log_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE conversation_members ADD COLUMN metadata_log_seq BIGINT NOT NULL DEFAULT 0;

-- THE BACKFILL TAKES A DRAW, NOT A READ, AND THE DIFFERENCE IS THE WHOLE POINT.
--
-- Setting existing rows to the counter's CURRENT value looks equivalent and is
-- not. A fully caught-up client holds a cursor equal to that value exactly:
-- Sync sets HighWater = head when has_more is false, and head is the counter's
-- committed value. Its next call sends `after = V`, and `metadata_log_seq > V`
-- is false for every backfilled row — so the most up-to-date clients, the ones
-- this backfill exists for, never receive their conversations again. A
-- conversation with no messages was never served to anyone before this
-- migration, so those clients cannot learn it exists. That is the DEFAULT 0
-- hole one value later.
--
-- Drawing puts the value above every cursor the server could have issued, by
-- construction. One draw for the whole table, which is what NOT UNIQUE buys.
--
-- CONDITIONAL, AND NOT AS AN ECONOMY. A data-modifying WITH runs exactly once
-- and to completion whether or not the outer query reads it, so the plain CTE
-- form draws even against an empty conversations table — which is every fresh
-- install and every test database. The counter's gaps carry no information, so
-- that is harmless to a client; it is not harmless to the tests that read the
-- counter as an absolute. CANT-19's two property tests assert that a sync
-- client sees log_seq 1, 2, 3 … with no hole, and CANT-13's up-down-up round
-- trip asserts the counter returns to 0 — a migration that silently consumes
-- the first value makes three green tests red for a reason that has nothing to
-- do with what they test.
--
-- So: draw only when there is something to backfill. A fresh database burns
-- nothing, and a database with conversations draws exactly one value for all of
-- them.
DO $$
DECLARE drawn BIGINT;
BEGIN
    IF EXISTS (SELECT 1 FROM conversations) THEN
        UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value INTO drawn;
        UPDATE conversations SET metadata_log_seq = drawn;
    END IF;
END $$;

-- `users` and `conversation_members` are NOT backfilled, and that is not an
-- omission. A user becomes visible to a viewer by joining a conversation, which
-- bumps THAT CONVERSATION and carries its members through the existing member
-- path — so only a rename ever needs to move a user's marker. A member row's
-- marker only has to beat a cursor once a receipt has advanced it, and until
-- then the conversation reaches the client through its messages.
--
-- NO INDEX. Both new predicates run under the membership join that already
-- scopes these queries to one viewer's conversations, and this deployment's
-- conversation count is a small trusted group's. An index here would be
-- speculative, and a speculative index is a thing nobody later dares remove.

COMMENT ON COLUMN conversations.metadata_log_seq IS
    'CANT-89. Drawn from log_counter when shared metadata changes — name, kind, membership, retention. /sync serves the conversation when this is above the caller''s cursor, which is the other half of SyncResponse.conversations'' promise. DEFAULT 0 means "never changed and never created through the draw helper": a row left at 0 is invisible to every cursor, which is why creating a conversation must draw and why a guard test bans writing these columns outside internal/store/metadata.go.';
COMMENT ON COLUMN conversation_members.metadata_log_seq IS
    'CANT-89, ruling 1. Drawn when THIS member''s read_seq advances, so their own other devices get first_unread_seq and muted on their next page and no other member is woken. Not drawn when a receipt does not advance the mark — MarkRead.Advanced already carries that distinction.';
COMMENT ON COLUMN users.metadata_log_seq IS
    'CANT-89. Drawn when display_name changes. /sync serves the user when this is above the caller''s cursor AND the caller shares a conversation with them (ruling 3): a rename reaches the people who can see the name, and nobody else.';
