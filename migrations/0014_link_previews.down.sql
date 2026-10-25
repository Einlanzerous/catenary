-- Reverses 0014. READ THIS BEFORE RUNNING IT: IT REFUSES WHILE A LINK CARD EXISTS.
--
-- storage_key cannot be made NOT NULL again, and the narrower kind CHECKs cannot
-- be restored, while a row of kind 'link' exists in attachments or uploads. The
-- rows in attachments are parts of authored messages, and this file does not
-- delete authored content on an operator's behalf: it counts the link rows and,
-- if there are any, raises an exception naming the count, and the transaction
-- the migrator wraps it in rolls back with the schema still at 0014 and every
-- row intact. The DDL below runs only when the count is zero.
--
-- To roll back over cards anyway, delete them FIRST, as a decision taken and
-- typed by a person rather than a side effect of a down:
--
--     DELETE FROM attachments WHERE kind = 'link';
--     DELETE FROM uploads     WHERE kind = 'link';
--
-- and then run this migration again. No MESSAGE is deleted by that either: a
-- message that carried a card keeps its text and its other attachments, which
-- is what a client that predates this kind already renders (it drops an
-- attachment kind it does not know). Position gaps are harmless. The image
-- blobs a card pointed at are files and are not touched by either step.
--
-- The 0012 down deletes the self conversations' own notes with a NOTICE, and
-- this file deliberately does not follow that precedent: those notes were their
-- owner's alone, and a card sits in a thread other people read.

DO $$
DECLARE
    link_attachments bigint;
    link_uploads bigint;
BEGIN
    SELECT count(*) INTO link_attachments FROM attachments WHERE kind = 'link';
    SELECT count(*) INTO link_uploads FROM uploads WHERE kind = 'link';
    IF link_attachments > 0 OR link_uploads > 0 THEN
        RAISE EXCEPTION USING
            MESSAGE = format('0014 down refused: %s link card(s) in attachments and %s in uploads would be destroyed', link_attachments, link_uploads),
            HINT = 'Rolling back over link cards deletes authored content. If that is the decision, run DELETE FROM attachments WHERE kind = ''link''; DELETE FROM uploads WHERE kind = ''link''; yourself, then run the down again. Messages are not affected either way.',
            ERRCODE = 'check_violation';
    END IF;
END
$$;

ALTER TABLE uploads DROP CONSTRAINT uploads_link_columns_only_on_link;
ALTER TABLE uploads DROP CONSTRAINT uploads_link_image_dims;
ALTER TABLE attachments DROP CONSTRAINT attachments_link_columns_only_on_link;
ALTER TABLE attachments DROP CONSTRAINT attachments_link_image_dims;
ALTER TABLE attachments DROP CONSTRAINT attachments_link_fields;
ALTER TABLE attachments DROP CONSTRAINT attachments_storage_key_present;
ALTER TABLE uploads DROP CONSTRAINT uploads_storage_key_present;

ALTER TABLE attachments ALTER COLUMN storage_key SET NOT NULL;
ALTER TABLE uploads ALTER COLUMN storage_key SET NOT NULL;

ALTER TABLE attachments
    DROP COLUMN link_url,
    DROP COLUMN link_title,
    DROP COLUMN link_description,
    DROP COLUMN link_site_name;
ALTER TABLE uploads
    DROP COLUMN link_url,
    DROP COLUMN link_title,
    DROP COLUMN link_description,
    DROP COLUMN link_site_name;

ALTER TABLE attachments DROP CONSTRAINT attachments_kind_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_kind_check CHECK (kind IN ('voice', 'image'));
ALTER TABLE uploads DROP CONSTRAINT uploads_kind_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_kind_check CHECK (kind IN ('voice', 'image'));

COMMENT ON COLUMN attachments.storage_key IS
    'Opaque storage key, NOT a URL. The wire''s url is derived from this at serve time, because whether it is a presigned R2 GET or a local path is CANT-47''s decision and a stored URL would be wrong the moment that lands.';
COMMENT ON COLUMN uploads.storage_key IS NULL;
