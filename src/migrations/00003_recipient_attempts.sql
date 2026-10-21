-- +goose Up
-- Per-row attempt budget. An unrecognised failure returns the row to the
-- queue, and the selection takes the first row by id: without a counter a row
-- with a persistent error holds the head of the queue forever and the campaign
-- does not advance.
ALTER TABLE recipients ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE recipients DROP COLUMN attempts;
