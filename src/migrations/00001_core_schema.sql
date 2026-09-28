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

-- Синглтон-паттерн: частичный уникальный индекс на постоянное значение
-- status ограничивает подходящие строки одной штукой, а не двумя частичный
-- предикатами по отдельному id. Гонка двух load_campaign разрешается на этом
-- индексе, а не в коде.
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

-- Строка аккаунта заводится один раз при первом старте процесса с данным
-- ACCOUNT_LABEL; уникальность метки не даёт второму контейнеру завести дубль.
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

-- Один контакт не встречается в одной кампании дважды: повторная пачка после
-- таймаута отсеивает дубль здесь, а не падает всей загрузкой.
CREATE UNIQUE INDEX recipients_campaign_contact_idx ON recipients (campaign_id, amo_contact_id);

CREATE TABLE stop_list (
    id BIGSERIAL PRIMARY KEY,
    amo_contact_id BIGINT,
    username TEXT,
    CONSTRAINT stop_list_identity_check CHECK (amo_contact_id IS NOT NULL OR username IS NOT NULL)
);

-- Один и тот же контакт или ник не должен попадать в стоп-лист дважды: иначе
-- ручное ведение списка молча плодит дубли строк с одним и тем же смыслом.
CREATE UNIQUE INDEX stop_list_contact_idx ON stop_list (amo_contact_id) WHERE amo_contact_id IS NOT NULL;
CREATE UNIQUE INDEX stop_list_username_idx ON stop_list (username) WHERE username IS NOT NULL;

-- +goose Down
DROP TABLE stop_list;
DROP TABLE recipients;
DROP TABLE accounts;
DROP TABLE campaigns;
