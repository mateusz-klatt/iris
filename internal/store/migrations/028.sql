-- When an active Execution's lease was last created or renewed (domain tick, ms),
-- so a hub stall can bound catch-up credit to one cap from that renewal.
ALTER TABLE Execution ADD COLUMN lease_renewed_tick_ms INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=28;
