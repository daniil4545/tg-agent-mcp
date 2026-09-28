-- +goose Up
-- Ник Telegram приходит в апдейте не всегда: короткое входящее несёт только id
-- отправителя. Без сохранённого id ответ такого человека не находит свою строку
-- и приглашение считается неотвеченным.
ALTER TABLE recipients ADD COLUMN tg_user_id BIGINT;

CREATE INDEX recipients_tg_user_id_idx ON recipients (tg_user_id) WHERE tg_user_id IS NOT NULL;

-- +goose Down
DROP INDEX recipients_tg_user_id_idx;
ALTER TABLE recipients DROP COLUMN tg_user_id;
