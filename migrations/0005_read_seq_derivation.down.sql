-- Restores 0002's wording verbatim. It describes a derivation CANT-83 removed
-- (see the up migration), but a down migration returns the schema to what the
-- previous version shipped rather than to what that version should have said.

COMMENT ON COLUMN conversation_members.read_seq IS
    'Highest seq this member has read. An author''s own send advances it, which is what makes first_unread_seq arithmetic rather than a scan (CANT-26).';
