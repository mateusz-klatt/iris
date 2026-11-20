-- An Execution's presence time alone no longer moves console cursors. Every hook call and
-- heartbeat writes last_seen_ms, so with ten busy agents an operator's cursor moved about
-- twice a second and the console refetched without end. The console shows no last_seen.
-- A changed repository, branch or state still moves the cursor; the WHEN clause also skips
-- an UPDATE that sets one of them to the value it already has.
DROP TRIGGER console_revision_execution_update;
CREATE TRIGGER console_revision_execution_update AFTER UPDATE OF repository_id,branch,state ON Execution
WHEN OLD.repository_id IS NOT NEW.repository_id OR OLD.branch IS NOT NEW.branch OR OLD.state IS NOT NEW.state BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;
PRAGMA user_version=26;
