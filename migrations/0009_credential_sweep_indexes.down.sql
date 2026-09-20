-- Both indexes go; neither carries data anything downstream depends on. No
-- IF EXISTS: 0008's down needed one for a documented, specific race in its
-- own history, and that is not a reason to make every later down forgiving of
-- a schema that already diverged from what MigrateDown thinks it is rolling
-- back.
DROP INDEX refresh_tokens_issued_at_idx;
DROP INDEX access_tokens_issued_at_idx;
