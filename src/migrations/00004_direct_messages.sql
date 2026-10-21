-- +goose Up
-- Log of manual touches: tg_send writes to strangers outside the campaign
-- queue, and without recording such a send tomorrow's campaign would message
-- the person a second time. The row is written before the network call:
-- "Telegram confirmed, the process died, no record" would leave the person
-- unmarked, a direct breach of the "one person, one message" guard.
CREATE TABLE direct_messages (
    id BIGSERIAL PRIMARY KEY,
    username TEXT NOT NULL,
    tg_user_id BIGINT,
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    text TEXT NOT NULL,
    random_id BIGINT NOT NULL,
    message_id BIGINT,
    cold BOOLEAN NOT NULL DEFAULT false,
    state TEXT NOT NULL DEFAULT 'pending',
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ,
    CONSTRAINT direct_messages_state_check CHECK (state IN ('pending','sent','failed'))
);

-- random_id is fresh on every call and uniqueness guards it: a dialog is full
-- of identical replies, and a repeated identifier would make Telegram swallow
-- the second message as a duplicate.
CREATE UNIQUE INDEX direct_messages_random_id_idx ON direct_messages (random_id);

-- The campaign filter queries the log by username for every queue row; failed
-- rows are left out of the index: the message certainly was not sent, so the
-- person is available again.
CREATE INDEX direct_messages_username_idx ON direct_messages (username) WHERE state <> 'failed';

-- Daily count of cold touches: the first touch of a stranger spends the same
-- daily cap of the account as a campaign does.
CREATE INDEX direct_messages_cold_idx ON direct_messages (created_at) WHERE cold AND state <> 'failed';

-- +goose Down
DROP TABLE direct_messages;
