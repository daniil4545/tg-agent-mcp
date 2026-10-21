-- +goose Up
CREATE TABLE campaigns (
    id BIGSERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    template_text TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    CONSTRAINT campaigns_status_check CHECK (status IN (
        'draft', 'active', 'stopped', 'done', 'cancelled'
    ))
);

-- Singleton pattern: a partial unique index on a constant status value
-- limits matching rows to one, with no per-id predicates. A race between two
-- load_campaign calls is resolved by this index, not in code.
CREATE UNIQUE INDEX campaigns_one_active_idx ON campaigns (status) WHERE status = 'active';
CREATE UNIQUE INDEX campaigns_one_draft_idx ON campaigns (status) WHERE status = 'draft';

CREATE TABLE accounts (
    id BIGSERIAL PRIMARY KEY,
    label TEXT NOT NULL,
    daily_limit INTEGER NOT NULL,
    stopped BOOLEAN NOT NULL DEFAULT false,
    stop_reason TEXT NOT NULL DEFAULT '',
    stopped_at TIMESTAMPTZ,
    CONSTRAINT accounts_daily_limit_check CHECK (daily_limit > 0)
);

-- The account row is created once, when a process with a given ACCOUNT_LABEL
-- starts for the first time; label uniqueness stops a second container from
-- creating a duplicate.
CREATE UNIQUE INDEX accounts_label_idx ON accounts (label);

CREATE TABLE recipients (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
    amo_contact_id BIGINT NOT NULL,
    amo_lead_id BIGINT NOT NULL DEFAULT 0,
    name TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'planned',
    reason TEXT NOT NULL DEFAULT '',
    account_id BIGINT REFERENCES accounts(id),
    random_id BIGINT NOT NULL,
    claimed_at TIMESTAMPTZ,
    sent_at TIMESTAMPTZ,
    replied_at TIMESTAMPTZ,
    CONSTRAINT recipients_state_check CHECK (state IN (
        'planned', 'sent', 'replied', 'undelivered', 'skipped', 'cancelled'
    ))
);

-- One contact appears at most once per campaign: a repeated batch after a
-- timeout drops the duplicate here instead of failing the whole load.
CREATE UNIQUE INDEX recipients_campaign_contact_idx ON recipients (campaign_id, amo_contact_id);

CREATE TABLE stop_list (
    id BIGSERIAL PRIMARY KEY,
    amo_contact_id BIGINT,
    username TEXT,
    CONSTRAINT stop_list_identity_check CHECK (amo_contact_id IS NOT NULL OR username IS NOT NULL)
);

-- The same contact or username must not enter the stop list twice: otherwise
-- manual upkeep of the list silently breeds duplicate rows with the same
-- meaning.
CREATE UNIQUE INDEX stop_list_contact_idx ON stop_list (amo_contact_id) WHERE amo_contact_id IS NOT NULL;
CREATE UNIQUE INDEX stop_list_username_idx ON stop_list (username) WHERE username IS NOT NULL;

-- +goose Down
DROP TABLE stop_list;
DROP TABLE recipients;
DROP TABLE accounts;
DROP TABLE campaigns;
