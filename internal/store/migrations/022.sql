-- A reduced shell snapshot (ADR 018) keeps its map and its scope here, apart from the full
-- snapshots in ShellSnapshot. An older hub reads only ShellSnapshot, so it never takes a
-- reduced map for a full one; the cascade lets it delete an Execution that has one.
CREATE TABLE IF NOT EXISTS ReducedShellSnapshot (
 execution_id TEXT PRIMARY KEY REFERENCES Execution(id) ON DELETE CASCADE,
 status_json TEXT NOT NULL,
 scope_json TEXT NOT NULL
);
PRAGMA user_version=22;
