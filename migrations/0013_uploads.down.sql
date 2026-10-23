-- Reverses 0013. Drops the uploads table: pending handles stop resolving and a
-- thumbnail can no longer be found from its image's key. No message is touched
-- (attachments does not reference uploads) and no blob file is removed.
DROP TABLE uploads;
