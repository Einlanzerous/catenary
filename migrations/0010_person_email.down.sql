-- Reverses 0010 in the opposite order it was added: the CHECK first, since
-- it names the column; then the index, which also names it; then the column
-- itself, once nothing else references it.
ALTER TABLE users DROP CONSTRAINT users_bot_has_no_email;
DROP INDEX users_email_lower_idx;
ALTER TABLE users DROP COLUMN email;
