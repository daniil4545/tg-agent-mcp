-- +goose Up
-- Журнал ручных касаний: tg_send пишет незнакомым людям мимо очереди кампании,
-- и без учёта такой отправки завтрашняя кампания напишет человеку второй раз.
-- Строка ложится до сетевого вызова: «Telegram подтвердил - процесс умер -
-- записи нет» оставило бы человека незакрытым, а это прямое нарушение рубежа
-- «один человек - одно сообщение».
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

-- random_id свежий на каждый вызов, и уникальность его стережёт: диалог полон
-- одинаковых реплик, и повтор идентификатора заставил бы Telegram проглотить
-- второе сообщение как дубль.
CREATE UNIQUE INDEX direct_messages_random_id_idx ON direct_messages (random_id);

-- Отсев кампании спрашивает журнал по нику на каждой строке очереди; failed в
-- индекс не входит - сообщение заведомо не ушло, человек снова доступен.
CREATE INDEX direct_messages_username_idx ON direct_messages (username) WHERE state <> 'failed';

-- Суточный счёт холодных касаний: первое касание незнакомца расходует тот же
-- дневной потолок аккаунта, что и рассылка.
CREATE INDEX direct_messages_cold_idx ON direct_messages (created_at) WHERE cold AND state <> 'failed';

-- +goose Down
DROP TABLE direct_messages;
