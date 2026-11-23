-- When an active Execution's lease was last created or renewed (domain tick, ms),
-- so a hub stall can bound catch-up credit to one cap from that renewal.
--
-- No backfill from deadline_tick_ms - lease is performed here, deliberately:
-- every upgrade restarts the hub, Open increments boot_epoch and places all
-- active Executions into recovery (recovery_deadline IS NOT NULL), stall credit
-- skips recovering rows and prior boot epochs, and resume writes
-- lease_renewed_tick_ms on the new boot's clock once the session reattaches.
-- Backfilling from a prior boot's deadline is meaningless because monotonic
-- ticks across restarts share no origin and could even yield negative values.
ALTER TABLE Execution ADD COLUMN lease_renewed_tick_ms INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=28;
