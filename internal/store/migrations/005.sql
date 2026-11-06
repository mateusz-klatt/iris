CREATE TABLE HostToken (
 name TEXT PRIMARY KEY,
 digest BLOB NOT NULL UNIQUE,
 created_ms INTEGER NOT NULL,
 revoked_ms INTEGER,
 last_used_ms INTEGER,
 requests INTEGER NOT NULL DEFAULT 0
);
PRAGMA user_version=5;
