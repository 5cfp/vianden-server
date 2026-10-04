-- Text channels and their messages (milestone M2).

-- +goose Up

CREATE TABLE channels (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 32),
    topic      TEXT NOT NULL DEFAULT '' CHECK (char_length(topic) <= 120),
    -- Only 'text' for now; 'voice' arrives in M7.
    type       TEXT NOT NULL DEFAULT 'text' CHECK (type IN ('text')),
    -- Order in the room list (lowest first).
    position   INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Names are unique ignoring case: "Games" and "games" cannot both exist.
CREATE UNIQUE INDEX channels_name_lower_idx ON channels (lower(name));

-- Every server starts with one room.
INSERT INTO channels (name, topic, position) VALUES ('General', 'Everything and nothing', 0);

CREATE TABLE messages (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Deleting a channel deletes its messages.
    channel_id BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    -- If an account is ever deleted, its messages stay, shown as "deleted user".
    author_id  BIGINT REFERENCES users (id) ON DELETE SET NULL,
    content    TEXT NOT NULL CHECK (char_length(content) BETWEEN 1 AND 4000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Makes "the newest N messages of channel X, older than message Y" fast,
-- however many messages the server has (used for history pagination).
CREATE INDEX messages_channel_id_id_idx ON messages (channel_id, id DESC);

-- +goose Down

DROP TABLE messages;
DROP TABLE channels;
