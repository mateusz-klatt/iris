-- When an idempotency record was made, so retention can drop it after a day (ADR 006).
-- Records from before this migration count from now.
ALTER TABLE Operation ADD COLUMN created_ms INTEGER NOT NULL DEFAULT 0;
UPDATE Operation SET created_ms=CAST(strftime('%s','now') AS INTEGER)*1000;
PRAGMA user_version=13;
