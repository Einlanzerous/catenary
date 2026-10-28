-- CANT-333 (CANT-308 row, CANT-331 ruling 0 option A) · 0015_link_final_host — the
-- host a link card's fetch finally landed on, when a redirect took it off the
-- site of the pasted URL (youtu.be -> www.youtube.com).
--
-- ADDITIVE, NO BACKFILL. Both tables gain one nullable text column. No existing
-- row is modified: every card already sent keeps NULL, which means "show url's
-- host alone", and a frozen card is authored content that no migration rewrites
-- or re-judges. There is no UPDATE in this file.
--
--   * link_final_host is a bare host: lowercase ASCII labels (an IDN as xn--
--     punycode) or a bracketed IPv6 literal, no scheme, port, path or query.
--     The redirect chain is the TARGET's choice, so the value is page-influenced;
--     the CHECK holds it to the same pattern as the wire schema's
--     LinkAttachment.final_host (and the three generated decoders), with the
--     same 253 cap. The Postgres ARE accepts the schema's JSON-Schema regex
--     verbatim, non-capturing group included.
--   * Stored, not derived: where one fetch's redirects led cannot be recomputed
--     from the pasted url, and the live target is the one source that must not be
--     consulted again. It freezes in the same snapshot as link_title.
--
-- LOCKS. ADD COLUMN of a nullable column with no default is a catalog-only
-- change. The two ADD CONSTRAINTs of the re-added *_link_columns_only_on_link
-- (and the new shape CHECK) validate every existing row, so each takes ACCESS
-- EXCLUSIVE on its table for one sequential scan, inside the migrator's single
-- transaction. The scan is bounded by the existing uploads and attachments row
-- counts for a small trusted group, and it is safe because the migrator runs
-- before the service accepts traffic on boot. NOT VALID + VALIDATE would not
-- shorten the lock inside one transaction, so it is not used.

ALTER TABLE uploads     ADD COLUMN link_final_host TEXT;
ALTER TABLE attachments ADD COLUMN link_final_host TEXT;

ALTER TABLE uploads ADD CONSTRAINT uploads_link_final_host_shape CHECK (
    link_final_host IS NULL OR (
        char_length(link_final_host) <= 253
        AND link_final_host ~ '^(?:(?:[a-z0-9_-]{1,63}\.)+[a-z0-9_-]{1,63}|\[[0-9a-f:.]+\])$'
    )
);
ALTER TABLE attachments ADD CONSTRAINT attachments_link_final_host_shape CHECK (
    link_final_host IS NULL OR (
        char_length(link_final_host) <= 253
        AND link_final_host ~ '^(?:(?:[a-z0-9_-]{1,63}\.)+[a-z0-9_-]{1,63}|\[[0-9a-f:.]+\])$'
    )
);

-- The link_* columns belong to a link and to nothing else: extend both 0014
-- constraints to cover the new column.
ALTER TABLE uploads DROP CONSTRAINT uploads_link_columns_only_on_link;
ALTER TABLE uploads ADD CONSTRAINT uploads_link_columns_only_on_link CHECK (
    kind = 'link' OR (
        link_url IS NULL AND link_title IS NULL
        AND link_description IS NULL AND link_site_name IS NULL
        AND link_final_host IS NULL
    )
);
ALTER TABLE attachments DROP CONSTRAINT attachments_link_columns_only_on_link;
ALTER TABLE attachments ADD CONSTRAINT attachments_link_columns_only_on_link CHECK (
    kind = 'link' OR (
        link_url IS NULL AND link_title IS NULL
        AND link_description IS NULL AND link_site_name IS NULL
        AND link_final_host IS NULL
    )
);

COMMENT ON COLUMN attachments.link_final_host IS
    'kind = link only (CANT-331). The host the fetch landed on after a cross-site redirect, set by the server at compose time and frozen; NULL when the fetch stayed on url''s site and on every card made before this column existed. A bare lowercase ASCII host or a bracketed IPv6 literal.';
