-- CANT-29 — the two indexes reuse detection needs, and nothing else.
--
-- NO NEW COLUMN, AND THAT IS WORTH SAYING FIRST. Detection needs to know WHEN a
-- token was rotated, and there is no `replaced_at`: the successor's `issued_at`
-- is that moment, and `refresh_tokens.replaced_by` is ON DELETE RESTRICT, so the
-- row a replaced token names cannot be deleted out from under the join.
-- `internal/store/schema_test.go` already calls that constraint "precisely the
-- evidence CANT-29's reuse detection reads". So the chain itself is the clock.
--
-- BOTH INDEXES HAVE A NAMED QUERY IN THIS TICKET, which is the bar 0007 set when
-- it deliberately shipped `family_id` unindexed: "the COLUMN has to be here
-- because adding it later rewrites the table; the INDEX does not, so it belongs
-- to the ticket whose query needs it." This is that ticket. 0006 states the
-- other half of the same rule — a speculative index is a thing nobody later
-- dares remove — and neither of these is speculative now.

-- Invalidation is one predicate over one column, which is the whole reason a
-- family is a column rather than a walk back up `replaced_by`: CANT-29 runs it
-- at the moment it has just decided something is wrong, and a walk is a loop
-- that can be interrupted half-done.
CREATE INDEX refresh_tokens_family_id_idx ON refresh_tokens (family_id);

-- THE SECOND ONE IS NOT DECORATION. Invalidating a family also revokes what the
-- family currently buys — the device's access tokens — because a `Done when`
-- that says the legitimate device is "forced to re-authenticate rather than
-- silently continuing" is not met by a device that keeps working for the
-- remainder of its fifteen minutes.
--
-- That write is `WHERE device_id = $1`, and `access_tokens` had no index on
-- device_id at all: its only index is the UNIQUE on `token_hash` that ruling 0's
-- point-read uses. Unindexed it is a sequential scan over a table that CANT-118
-- records as growing without bound — roughly 96 rows per device per day, since
-- every rotation inserts one and nothing sweeps them yet. The scan would be
-- imperceptible on the day this lands and would rot quietly, which is the worst
-- shape for a query that only runs while somebody is being locked out.
CREATE INDEX access_tokens_device_id_idx ON access_tokens (device_id);

COMMENT ON INDEX refresh_tokens_family_id_idx IS
    'CANT-29. Reuse detection invalidates a whole family by this column: UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL. Deferred from 0007 on the rule that an index belongs to the ticket whose query needs it.';
COMMENT ON INDEX access_tokens_device_id_idx IS
    'CANT-29. Family invalidation revokes the device''s access tokens in the same transaction, so the legitimate device re-authenticates rather than silently continuing for the rest of its access TTL. Also the index CANT-118''s sweep will want.';
