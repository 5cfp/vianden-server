-- Voice channels (milestone M7). A channel is either "text" or "voice" (voice only:
-- no messages). The type is chosen when the channel is created and never changes.

-- +goose Up

ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text', 'voice'));

-- +goose Down

DELETE FROM channels WHERE type = 'voice';
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text'));
