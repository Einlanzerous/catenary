-- Reverses 0012. READ THIS BEFORE RUNNING IT: THIS DELETES AUTHORED MESSAGES.
--
-- messages.conversation_id and conversation_members.conversation_id are
-- ON DELETE RESTRICT, so the self conversations cannot be removed (and the
-- narrower CHECK cannot be restored while one exists) without first deleting
-- what points at them. Every statement below is bounded by the same predicate,
--
--     conversation_id IN (SELECT id FROM conversations WHERE kind = 'self')
--
-- so the rows removed are the self conversations' own contents and nothing
-- else: the messages in them (their attachments go with the message, ON DELETE
-- CASCADE), their one membership row each, and then the conversations. A direct
-- or a group conversation and everything in it is not touched.
--
-- What they held is notes their owner wrote to themselves. A person who does not
-- want that loses nothing by rolling FORWARD instead: 0012 is additive, and a
-- build that predates `self` renders such a conversation as a group.
--
-- The number of messages deleted is counted here, at down time, and raised as a
-- NOTICE so the operator's output carries it rather than an assumption.

DO $$
DECLARE
    doomed bigint;
BEGIN
    SELECT count(*) INTO doomed
      FROM messages
     WHERE conversation_id IN (SELECT id FROM conversations WHERE kind = 'self');
    RAISE NOTICE '0012 down: deleting % message(s) from kind = ''self'' conversations', doomed;
END
$$;

DELETE FROM messages
 WHERE conversation_id IN (SELECT id FROM conversations WHERE kind = 'self');
DELETE FROM conversation_members
 WHERE conversation_id IN (SELECT id FROM conversations WHERE kind = 'self');
DELETE FROM conversations WHERE kind = 'self';

DROP INDEX conversations_self_key_idx;

ALTER TABLE conversations DROP CONSTRAINT conversations_kind_check;
ALTER TABLE conversations
    ADD CONSTRAINT conversations_kind_check CHECK (kind IN ('direct', 'group'));
