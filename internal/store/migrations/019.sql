-- Owner setup/recovery is not an account acting as itself. Preserve the target
-- while recording the owner credential as a distinct actor kind.
CREATE TABLE ConsoleActionNext (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 actor_account_id TEXT REFERENCES ConsoleAccount(id),
 target_account_id TEXT REFERENCES ConsoleAccount(id),
 action TEXT NOT NULL,
 outcome TEXT NOT NULL,
 created_ms INTEGER NOT NULL,
 actor_kind TEXT NOT NULL DEFAULT 'account' CHECK(actor_kind IN ('account','owner')),
 CHECK((actor_kind='account' AND actor_account_id IS NOT NULL)
    OR (actor_kind='owner' AND actor_account_id IS NULL))
);
INSERT INTO ConsoleActionNext(id,actor_account_id,target_account_id,action,outcome,created_ms)
 SELECT id,actor_account_id,target_account_id,action,outcome,created_ms FROM ConsoleAction;
DROP TABLE ConsoleAction;
ALTER TABLE ConsoleActionNext RENAME TO ConsoleAction;
PRAGMA user_version=19;
