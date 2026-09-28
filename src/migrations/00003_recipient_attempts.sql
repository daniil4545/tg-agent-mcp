-- +goose Up
-- Бюджет попыток строки. Неопознанный отказ возвращает строку в очередь, а
-- выборка берёт первую по id: без счётчика строка со стабильной ошибкой держит
-- голову очереди вечно и кампания не двигается.
ALTER TABLE recipients ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE recipients DROP COLUMN attempts;
