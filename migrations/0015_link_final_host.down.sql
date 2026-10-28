-- Reverses 0015. A plain DROP COLUMN, and it does NOT refuse, unlike 0014's down.
--
-- 0014's down refuses because it would destroy link cards, which are authored
-- parts of messages. This column is different in kind: it is nullable, additive
-- and DERIVED from one fetch's redirect chain, and a card without it is a valid,
-- complete card (absent means "show url's host alone", which is what every card
-- sent before 0015 already renders). Dropping it loses a second-host hint, never
-- the card, its url, title, image or the message. Nothing else depends on it in
-- the schema; the code that reads it ships with a client that tolerates absence.
--
-- Restores the 0014 *_link_columns_only_on_link constraints exactly. Same locks
-- as the up: each re-added CHECK scans its table once under ACCESS EXCLUSIVE.

ALTER TABLE uploads DROP CONSTRAINT uploads_link_columns_only_on_link;
ALTER TABLE uploads DROP CONSTRAINT uploads_link_final_host_shape;
ALTER TABLE attachments DROP CONSTRAINT attachments_link_columns_only_on_link;
ALTER TABLE attachments DROP CONSTRAINT attachments_link_final_host_shape;

ALTER TABLE uploads     DROP COLUMN link_final_host;
ALTER TABLE attachments DROP COLUMN link_final_host;

ALTER TABLE uploads ADD CONSTRAINT uploads_link_columns_only_on_link CHECK (
    kind = 'link' OR (
        link_url IS NULL AND link_title IS NULL
        AND link_description IS NULL AND link_site_name IS NULL
    )
);
ALTER TABLE attachments ADD CONSTRAINT attachments_link_columns_only_on_link CHECK (
    kind = 'link' OR (
        link_url IS NULL AND link_title IS NULL
        AND link_description IS NULL AND link_site_name IS NULL
    )
);
