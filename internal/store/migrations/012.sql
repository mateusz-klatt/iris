-- When a hook last reached the hub for an Execution: a session registered by a hook, an
-- automatic claim, or a shell reconcile. Heartbeats from the MCP shim and the monitor do not
-- count, so doctor can say when a client's hooks last ran (gap review C4).
ALTER TABLE Execution ADD COLUMN hook_seen_ms INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=12;
