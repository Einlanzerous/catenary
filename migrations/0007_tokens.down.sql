-- Order matters: enrolment_tokens references devices, and access_tokens and
-- refresh_tokens reference both devices and users. Nothing outside this
-- migration references anything in it, so dropping the three tables first
-- leaves users.kind free to go.
DROP TABLE enrolment_tokens;
DROP TABLE refresh_tokens;
DROP TABLE access_tokens;
ALTER TABLE users DROP COLUMN kind;
