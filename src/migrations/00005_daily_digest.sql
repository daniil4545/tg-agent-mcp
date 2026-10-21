-- +goose Up
-- Lock and log of the evening digest in one row. There are several senders,
-- all running the same loop on a shared database: without a lock each of them
-- would send the digest. The key is the pair (day, recipient), not the day
-- alone: after a partial success a retry must resend only to those who did not
-- get it, otherwise the first recipient would receive the digest twice.
CREATE TABLE daily_digest (
    id BIGSERIAL PRIMARY KEY,
    day DATE NOT NULL,
    username TEXT NOT NULL,
    campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    created_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ,
    CONSTRAINT daily_digest_day_user_idx UNIQUE (day, username)
);

-- +goose Down
DROP TABLE daily_digest;
