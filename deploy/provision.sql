-- Catenary · its own database and its own role on the shared Postgres 16.
--
-- CANT-13 Ruling 3. A shared database means another service holds a credential
-- against Catenary's tables, and D1 — no end-to-end encryption, the server can
-- read everything — makes that a larger statement here than elsewhere.
--
-- RUN THIS ONCE, BY HAND, AS A SUPERUSER. It is deliberately not a migration:
-- migrations run as the catenary role, and a role cannot create itself.
--
--   psql -h <host> -U postgres -f deploy/provision.sql -v password="$(read from Signet)"
--
-- The password NEVER appears in this file, in a compose file, or in the repo.
-- Signet holds the copy of record; compose reads it from there.

\set ON_ERROR_STOP on

-- LOGIN and nothing else. Every capability below is granted explicitly, and
-- CHRN-78's lesson is why: a grant is justified by what it actually DOES, not
-- by what the comment next to it says it does. Each line here is checked
-- against the database by the grants test rather than trusted.
CREATE ROLE catenary WITH LOGIN PASSWORD :'password';

-- NOSUPERUSER, NOCREATEDB, NOCREATEROLE are the defaults for CREATE ROLE and
-- are restated so that "we did not grant these" is written down rather than
-- inferred from an absence.
ALTER ROLE catenary NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;

CREATE DATABASE catenary OWNER catenary;

-- Nobody else's default search path should find these tables, and no other
-- estate role should be able to read them.
REVOKE ALL ON DATABASE catenary FROM PUBLIC;
GRANT CONNECT ON DATABASE catenary TO catenary;

\connect catenary

-- The service owns its own schema, because the migrator creates tables and
-- CREATE on the schema is the permission that needs. This is the one broad
-- grant, and it is broad on purpose: the alternative is a second role that
-- migrates, which is more moving parts than this deployment earns.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO catenary;
GRANT ALL ON SCHEMA public TO catenary;

-- What this does NOT grant, stated so the absence is a decision:
--   * no CREATEDB, so the service cannot make itself a second database;
--   * no CREATEROLE, so a compromised credential cannot mint another;
--   * no SUPERUSER and no BYPASSRLS;
--   * no access to any other estate database — CONNECT is granted on this one
--     only, and PUBLIC's implicit CONNECT was revoked above.
