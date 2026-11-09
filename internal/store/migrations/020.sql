-- Separate reviewed assignment transfer. Preserve ledger entries if a repository
-- is forgotten: replays must never recreate a removed or subsequently edited grant.
CREATE TABLE ConsoleAssignmentImport (
 source_id TEXT NOT NULL,
 source_row_id TEXT NOT NULL,
 account_id TEXT NOT NULL REFERENCES ConsoleAccount(id),
 repository_id TEXT NOT NULL,
 fingerprint_version INTEGER NOT NULL CHECK(fingerprint_version=1),
 source_fingerprint BLOB NOT NULL CHECK(length(source_fingerprint)=32),
 created_ms INTEGER NOT NULL,
 PRIMARY KEY(source_id,source_row_id)
);
PRAGMA user_version=20;
