-- Replies and edited messages (milestone M6, step 1).

-- +goose Up

ALTER TABLE messages
    -- The message this one replies to (same channel; checked by the server). If that row
    -- ever disappears, the reply stays and simply loses its quote.
    ADD COLUMN reply_to_id BIGINT REFERENCES messages (id) ON DELETE SET NULL,
    -- When the author last edited the text (NULL = never edited).
    ADD COLUMN edited_at TIMESTAMPTZ;

-- Deleting messages (e.g. a whole channel) must find the replies pointing at them quickly.
CREATE INDEX messages_reply_to_idx ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;

-- +goose Down

DROP INDEX messages_reply_to_idx;
ALTER TABLE messages DROP COLUMN edited_at, DROP COLUMN reply_to_id;
