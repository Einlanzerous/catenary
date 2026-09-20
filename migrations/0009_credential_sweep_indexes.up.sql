-- CANT-118 — the index the sweep's own predicate needs, and nothing else.
--
-- ADDITIVE ONLY, on 0008's own rule: an index belongs to the ticket whose
-- query needs it. SweepCredentials scans each table oldest issued_at first
-- and stops once it has filled one batch (internal/store/sweep.go); without
-- an index on issued_at that is a full sequential scan and sort on every
-- hourly pass, over precisely the two tables 0008's own comment records as
-- growing without bound — ~96 rows/device/day, and nothing swept them before
-- this ticket. Left unindexed, the query that is supposed to bring that
-- growth back under control would itself get slower as the backlog grew.
CREATE INDEX access_tokens_issued_at_idx ON access_tokens (issued_at);
CREATE INDEX refresh_tokens_issued_at_idx ON refresh_tokens (issued_at);

COMMENT ON INDEX access_tokens_issued_at_idx IS
    'CANT-118. SweepCredentials scans oldest-issued-first for rows past SpentCredentialRetention; this keeps that an index scan rather than a sort over the whole table.';
COMMENT ON INDEX refresh_tokens_issued_at_idx IS
    'CANT-118. Same as access_tokens_issued_at_idx, and also what makes oldest-first deletion cheap — the ordering that keeps refresh_tokens.replaced_by from ever dangling (sweep.go).';
