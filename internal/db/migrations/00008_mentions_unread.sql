-- Mentions and unread tracking (milestone M6, step 2).

-- +goose Up

-- Who a message pings: @username mentions, and the author of the message it replies to.
CREATE TABLE message_mentions (
    message_id BIGINT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, user_id)
);
CREATE INDEX message_mentions_user_idx ON message_mentions (user_id, message_id);

-- @everyone (only counts when the author was allowed to use it).
ALTER TABLE messages ADD COLUMN mentions_everyone BOOLEAN NOT NULL DEFAULT false;

-- The newest message each user has read in each channel. Everything after it is unread.
CREATE TABLE read_states (
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_id   BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    last_read_id BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, channel_id)
);

-- Existing users start with everything read (otherwise all old history would show as unread).
INSERT INTO read_states (user_id, channel_id, last_read_id)
SELECT u.id, c.id, COALESCE((SELECT MAX(m.id) FROM messages m WHERE m.channel_id = c.id), 0)
FROM users u CROSS JOIN channels c;

-- +goose Down

DROP TABLE read_states;
ALTER TABLE messages DROP COLUMN mentions_everyone;
DROP TABLE message_mentions;
