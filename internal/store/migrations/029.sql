-- ADR 037: a repository may opt into worktree-scoped automatic claims. The default
-- stays repository-wide. Existing rows are not backfilled: a null worktree key
-- remains repository-wide, and a legacy claim's display location stays unknown.
-- WorktreeIdentity is independent of WorktreeAlias. Replacing an alias when a
-- remote or root changes must not mint a new key for the same host and directory id.
ALTER TABLE Repository ADD COLUMN auto_claim_scope TEXT NOT NULL DEFAULT 'repository' CHECK(auto_claim_scope IN ('repository','worktree'));
ALTER TABLE Execution ADD COLUMN worktree_key TEXT;
ALTER TABLE Claim ADD COLUMN worktree_key TEXT;
ALTER TABLE Claim ADD COLUMN worktree_path TEXT;
ALTER TABLE Claim ADD COLUMN branch TEXT;
ALTER TABLE Waiter ADD COLUMN worktree_key TEXT;
ALTER TABLE Event ADD COLUMN worktree_key TEXT;
CREATE TABLE WorktreeIdentity (
 host_id TEXT NOT NULL, worktree_id TEXT NOT NULL, worktree_key TEXT NOT NULL UNIQUE,
 PRIMARY KEY (host_id, worktree_id)
);
-- A claim's worktree, branch and key are what the console shows. Renewing an expiry
-- still does not move a cursor; changing any of those three does.
DROP TRIGGER console_revision_claim_update;
CREATE TRIGGER console_revision_claim_update AFTER UPDATE OF execution_id,repository_id,kind,path,origin,revoked_ms,worktree_key,worktree_path,branch ON Claim
WHEN OLD.execution_id IS NOT NEW.execution_id OR OLD.repository_id IS NOT NEW.repository_id OR OLD.kind IS NOT NEW.kind OR OLD.path IS NOT NEW.path OR OLD.origin IS NOT NEW.origin OR OLD.revoked_ms IS NOT NEW.revoked_ms OR OLD.worktree_key IS NOT NEW.worktree_key OR OLD.worktree_path IS NOT NEW.worktree_path OR OLD.branch IS NOT NEW.branch BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;
PRAGMA user_version=29;
