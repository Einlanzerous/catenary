-- The counter draw the up migration took is NOT reversed, and cannot be.
-- log_counter is monotonic and sparse by design — Invariant 1 says its gaps
-- carry no information — so a consumed value is not a leak, it is the normal
-- shape of the log.
ALTER TABLE users                DROP COLUMN metadata_log_seq;
ALTER TABLE conversation_members DROP COLUMN metadata_log_seq;
ALTER TABLE conversations        DROP COLUMN metadata_log_seq;
