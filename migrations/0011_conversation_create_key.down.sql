-- Reverses 0011 in the opposite order: the index names the column, so it goes
-- first.
DROP INDEX conversations_create_key_idx;
ALTER TABLE conversations DROP COLUMN create_key;
