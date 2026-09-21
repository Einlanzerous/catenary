-- CANT-33 ruling 3 / CANT-130 · 0010_person_email — the identity key Purser
-- looks a person up by.
--
-- ADDITIVE ONLY, on 0007's and 0008's own rule: nothing here rewrites an
-- existing row, and `email` starts NULL for every row this database already
-- holds — the people `soakrig provision` inserted straight into the table
-- (CANT-109), and every test fixture that has ever inserted a user by hand.
--
-- NOT A PERSON-REQUIRES-EMAIL CHECK. Rev 1 of CANT-33's plan wanted one "for
-- rows created from now on", and that cannot be written: a CHECK reads a
-- row's current values, not the moment it was created, so a plain CHECK here
-- would fail this MIGRATION itself against every email-less person already
-- in the table. Migrations auto-apply on boot (migrations/migrations.go), so
-- a migration that fails against the deployed database is a crash loop, not
-- a rejected commit. Person-has-email is an invariant of EnsurePerson,
-- enforced and tested in the store (internal/store/persons.go) — not here,
-- and not ever as a CHECK on this table.
ALTER TABLE users ADD COLUMN email TEXT;

COMMENT ON COLUMN users.email IS
    'CANT-33 ruling 3 / CANT-130. Nullable: every person soakrig provisioned before this migration has none, and EnsurePerson — not a CHECK — is what starts requiring one, for a person it creates itself. Never on the wire: no wire type carries it and none should, under invariant 3 and D1''s honesty argument. A bot holds none; see the CHECK below.';

-- Case-insensitive and partial, so two people can never register the same
-- address folded, and an email-less person never contends for the index at
-- all — this is a uniqueness rule, not a presence rule.
CREATE UNIQUE INDEX users_email_lower_idx ON users (lower(email)) WHERE email IS NOT NULL;

COMMENT ON INDEX users_email_lower_idx IS
    'CANT-130. PersonByEmail and EnsurePerson both fold case before they look up or insert; this is the constraint that turns a fold-then-collide race into a 23505 instead of two rows holding the same address under different spellings. internal/store/persons.go tells this apart from users_handle_key BY CONSTRAINT NAME, the way isTokenHashCollision already does for refresh_tokens.';

-- ONE CHECK, and the only one this migration adds. Purser provisions people
-- and never bots (CANT-73's admin surface mints those, on a completely
-- separate path), so a bot holding an email is not a state this schema
-- allows, rather than a state the store merely declines to create.
ALTER TABLE users ADD CONSTRAINT users_bot_has_no_email
    CHECK (kind <> 'bot' OR email IS NULL);

COMMENT ON CONSTRAINT users_bot_has_no_email ON users IS
    'CANT-130. A bot is never identified by an email address — Purser provisions people, never bots — so this is enforced at the row rather than left to every future write path to remember.';

-- NO lower(handle) INDEX, on the same crash-loop argument as the missing
-- person-requires-email CHECK above. internal/store/persons.go's chooseHandle
-- states the rule instead — de-duplication folds case against every user,
-- persons and bots alike — precisely because an index that failed to build
-- against handles this deployment already holds would be the same failure
-- mode this migration was rewritten to avoid.
