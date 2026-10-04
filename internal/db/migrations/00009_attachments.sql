-- File and image uploads (milestone M6, step 3).

-- +goose Up

-- One uploaded file. It is uploaded first (message_id NULL, only its uploader can see it),
-- then attached to a message when that is sent. Unattached uploads are removed after an hour.
CREATE TABLE attachments (
    id           BIGSERIAL PRIMARY KEY,
    uploader_id  BIGINT REFERENCES users (id) ON DELETE SET NULL,
    message_id   BIGINT REFERENCES messages (id) ON DELETE CASCADE,
    -- Shown to users only. Never used as a path on disk.
    filename     TEXT NOT NULL CHECK (char_length(filename) BETWEEN 1 AND 200),
    -- Decided by the server from the file's content, never taken from the client.
    content_type TEXT NOT NULL,
    size         BIGINT NOT NULL CHECK (size > 0),
    width        INT,  -- images only
    height       INT,
    -- Random name of the file on disk (64 hex characters).
    storage_key  TEXT NOT NULL UNIQUE CHECK (storage_key ~ '^[0-9a-f]{64}$'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX attachments_message_idx ON attachments (message_id);
CREATE INDEX attachments_pending_idx ON attachments (uploader_id, created_at) WHERE message_id IS NULL;

-- A message may now be only attachments (no text). "Text or attachments" is checked by the server.
ALTER TABLE messages DROP CONSTRAINT messages_content_check;
ALTER TABLE messages ADD CONSTRAINT messages_content_check CHECK (
    (deleted_at IS NULL AND char_length(content) <= 4000)
    OR (deleted_at IS NOT NULL AND content = '')
);

-- +goose Down

DELETE FROM messages WHERE deleted_at IS NULL AND content = '';
ALTER TABLE messages DROP CONSTRAINT messages_content_check;
ALTER TABLE messages ADD CONSTRAINT messages_content_check CHECK (
    (deleted_at IS NULL AND char_length(content) BETWEEN 1 AND 4000)
    OR (deleted_at IS NOT NULL AND content = '')
);
DROP TABLE attachments;
