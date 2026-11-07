-- The contact for repo: mail (ADR 004). Not a declared foreign key: forget clears it
-- explicitly when its agent goes, which is the ON DELETE SET NULL rule the ADR names,
-- and a plain column can be dropped by the migration tests.
ALTER TABLE Repository ADD COLUMN contact_agent_id TEXT;
PRAGMA user_version=9;
