-- Who can see and who can write in each channel (milestone M5): a minimum role for each.

-- +goose Up

ALTER TABLE channels
    ADD COLUMN view_role TEXT NOT NULL DEFAULT 'member'
        CHECK (view_role IN ('owner', 'admin', 'moderator', 'member')),
    ADD COLUMN send_role TEXT NOT NULL DEFAULT 'member'
        CHECK (send_role IN ('owner', 'admin', 'moderator', 'member')),
    -- Writing needs at least the role needed to see (you cannot write where you cannot read).
    ADD CONSTRAINT channels_send_not_below_view CHECK (
        array_position(ARRAY['member', 'moderator', 'admin', 'owner'], send_role) >=
        array_position(ARRAY['member', 'moderator', 'admin', 'owner'], view_role)
    );

-- +goose Down

ALTER TABLE channels DROP CONSTRAINT channels_send_not_below_view;
ALTER TABLE channels DROP COLUMN send_role, DROP COLUMN view_role;
