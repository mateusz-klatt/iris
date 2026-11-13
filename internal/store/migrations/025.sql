-- ADR 006 amendment: a copy made by `iris admin backup` carries this row, and the first start on
-- that file runs on a new stream, which consumes the row. IF NOT EXISTS: a backup of a schema-24
-- database creates the table and its row itself, and this migration must keep that row.
CREATE TABLE IF NOT EXISTS RestoreMarker (
 id INTEGER PRIMARY KEY CHECK(id=1), made_ms INTEGER NOT NULL
);
PRAGMA user_version=25;
