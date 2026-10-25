-- CANT-321 (CANT-308 row 2) · 0014_link_previews — a link preview card is a new
-- attachment kind, written by the same attachment insert a send already does and
-- bound to the message by the same single-use handle an image upload is.
--
-- ADDITIVE. No existing row is touched, so there is no backfill and no lock
-- beyond the DDL, and the table sizes are the existing attachment counts.
--
--   * uploads.kind and attachments.kind gain 'link'.
--   * Both tables gain four nullable text columns for the card: the URL as the
--     sender typed it, and the title, description and site name the TARGET PAGE
--     chose. They are stored, not derived: the card is a snapshot of what the
--     page said when the sender composed the message, and the live page is the
--     one source that must not be consulted.
--   * storage_key becomes NULLABLE on both, and only a link may omit it: a card
--     whose page had no usable image has no blob. The card's image, when there
--     is one, is an ordinary media blob keyed by its SHA-256, with its
--     thumbnail on the kept uploads row exactly as for an image.
--
-- READERS OF A NULL storage_key. Making the column nullable breaks every reader
-- that assumes a string; each one is listed in the CANT-308 plan and is handled
-- in the same change as this file. The collection predicate in CANT-47 section 5
-- is CANT-307's and is unbuilt: a predicate written as
-- `blob_key NOT IN (SELECT storage_key FROM attachments)` goes unknown for every
-- row the moment one NULL is in the list, and collection would silently stop.
-- It must use NOT EXISTS or filter the NULLs.

ALTER TABLE uploads DROP CONSTRAINT uploads_kind_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_kind_check CHECK (kind IN ('voice', 'image', 'link'));
ALTER TABLE attachments DROP CONSTRAINT attachments_kind_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_kind_check CHECK (kind IN ('voice', 'image', 'link'));

ALTER TABLE uploads
    ADD COLUMN link_url         TEXT,
    ADD COLUMN link_title       TEXT,
    ADD COLUMN link_description TEXT,
    ADD COLUMN link_site_name   TEXT;
ALTER TABLE attachments
    ADD COLUMN link_url         TEXT,
    ADD COLUMN link_title       TEXT,
    ADD COLUMN link_description TEXT,
    ADD COLUMN link_site_name   TEXT;

ALTER TABLE uploads     ALTER COLUMN storage_key DROP NOT NULL;
ALTER TABLE attachments ALTER COLUMN storage_key DROP NOT NULL;

-- Only a link may have no blob. A voice or image row with a NULL key would be a
-- message whose media cannot be served, and nothing but the application would
-- have been standing between that and a committed row.
ALTER TABLE uploads ADD CONSTRAINT uploads_storage_key_present
    CHECK (storage_key IS NOT NULL OR kind = 'link');
ALTER TABLE attachments ADD CONSTRAINT attachments_storage_key_present
    CHECK (storage_key IS NOT NULL OR kind = 'link');

-- LinkAttachment requires `url`, and carries image_width and image_height
-- together or not at all; a card that names an image has its dimensions, since
-- the thread reserves the row's height from them before a byte arrives. The
-- link_* columns belong to a link and to nothing else.
ALTER TABLE attachments ADD CONSTRAINT attachments_link_fields CHECK (
    kind <> 'link' OR link_url IS NOT NULL
);
ALTER TABLE attachments ADD CONSTRAINT attachments_link_image_dims CHECK (
    kind <> 'link' OR (
        (width IS NULL) = (height IS NULL)
        AND (storage_key IS NULL OR width IS NOT NULL)
    )
);
ALTER TABLE attachments ADD CONSTRAINT attachments_link_columns_only_on_link CHECK (
    kind = 'link' OR (
        link_url IS NULL AND link_title IS NULL
        AND link_description IS NULL AND link_site_name IS NULL
    )
);

ALTER TABLE uploads ADD CONSTRAINT uploads_link_image_dims CHECK (
    kind <> 'link' OR (
        (width IS NULL) = (height IS NULL)
        AND (storage_key IS NULL OR width IS NOT NULL)
    )
);
ALTER TABLE uploads ADD CONSTRAINT uploads_link_columns_only_on_link CHECK (
    kind = 'link' OR (
        link_url IS NULL AND link_title IS NULL
        AND link_description IS NULL AND link_site_name IS NULL
    )
);

COMMENT ON COLUMN attachments.link_url IS
    'kind = link only (CANT-308). The URL as the sender typed it; the host a client shows is derived from it.';
COMMENT ON COLUMN attachments.link_title IS
    'kind = link only. Text the TARGET PAGE chose, sanitized and length-capped by the fetcher. A snapshot; never refetched.';
COMMENT ON COLUMN attachments.storage_key IS
    'Opaque storage key, NOT a URL. The wire''s url is derived from this at serve time, because whether it is a presigned R2 GET or a local path is CANT-47''s decision. NULL only for kind = link with no image (CANT-308): such a row references no blob, so any reader or collection predicate must treat NULL as "no blob" and must not use NOT IN over this column.';
COMMENT ON COLUMN uploads.storage_key IS
    'The SHA-256 of the stored bytes, also the blob''s name. NULL only for kind = link with no image (CANT-308); see attachments.storage_key.';
