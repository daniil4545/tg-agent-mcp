-- +goose Up
-- Замок и журнал вечерней сводки в одной строке. Отправщиков несколько, все
-- крутят один цикл на общей базе: без замка сводку отправил бы каждый.
-- Ключ - пара (день, адресат), а не день целиком: при частичном успехе повтор
-- обязан дослать только тем, кому не ушло, иначе первый получит сводку дважды.
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
