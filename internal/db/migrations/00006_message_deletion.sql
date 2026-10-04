-- Deleted messages (milestone M5): the text is erased for real, a placeholder row stays.

-- +goose Up

ALTER TABLE messages
    ADD COLUMN deleted_at TIMESTAMPTZ,
    ADD COLUMN deleted_by BIGINT REFERENCES users (id) ON DELETE SET NULL;

-- A live message has 1-4000 characters; a deleted one has none (its text is gone).
ALTER TABLE messages DROP CONSTRAINT messages_content_check;
ALTER TABLE messages ADD CONSTRAINT messages_content_check CHECK (
    (deleted_at IS NULL AND char_length(content) BETWEEN 1 AND 4000)
    OR (deleted_at IS NOT NULL AND content = '')
);

-- +goose Down

DELETE FROM messages WHERE deleted_at IS NOT NULL;
ALTER TABLE messages DROP CONSTRAINT messages_content_check;
ALTER TABLE messages ADD CONSTRAINT messages_content_check CHECK (char_length(content) BETWEEN 1 AND 4000);
ALTER TABLE messages DROP COLUMN deleted_by, DROP COLUMN deleted_at;
