-- +goose Up
-- A Telegram username is not always present in an update: a short incoming
-- message carries only the sender id. Without a stored id the reply of such a
-- person does not find its row, and the invitation counts as unanswered.
ALTER TABLE recipients ADD COLUMN tg_user_id BIGINT;

CREATE INDEX recipients_tg_user_id_idx ON recipients (tg_user_id) WHERE tg_user_id IS NOT NULL;

-- +goose Down
DROP INDEX recipients_tg_user_id_idx;
ALTER TABLE recipients DROP COLUMN tg_user_id;
