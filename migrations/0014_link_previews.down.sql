-- Reverses 0014. READ THIS BEFORE RUNNING IT: THIS DELETES LINK CARDS.
--
-- storage_key cannot be made NOT NULL again, and the narrower kind CHECKs cannot
-- be restored, while a link row exists. So every row of kind 'link' is deleted
-- from attachments and uploads first. No MESSAGE is deleted: a message that
-- carried a card keeps its text and its other attachments and loses only the
-- card, which is what a client that predates this kind already renders (it drops
-- an attachment kind it does not know). Position gaps are harmless.
--
-- The image blobs a card pointed at are files and are not touched.
--
-- The number of rows deleted is counted here, at down time, and raised as a
-- NOTICE so the operator's output carries it rather than an assumption.

DO $$
DECLARE
    doomed_attachments bigint;
    doomed_uploads bigint;
BEGIN
    SELECT count(*) INTO doomed_attachments FROM attachments WHERE kind = 'link';
    SELECT count(*) INTO doomed_uploads FROM uploads WHERE kind = 'link';
    RAISE NOTICE '0014 down: deleting % link card(s) from attachments and % from uploads', doomed_attachments, doomed_uploads;
END
$$;

DELETE FROM attachments WHERE kind = 'link';
DELETE FROM uploads WHERE kind = 'link';

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
