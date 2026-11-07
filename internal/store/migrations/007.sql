CREATE TABLE IF NOT EXISTS Waiter (
 execution_id TEXT NOT NULL REFERENCES Execution(id), repository_id TEXT NOT NULL REFERENCES Repository(id),
 path_key TEXT NOT NULL, kind TEXT NOT NULL, expires_ms INTEGER NOT NULL,
 PRIMARY KEY(execution_id,path_key,kind)
);
CREATE INDEX IF NOT EXISTS waiter_repo ON Waiter(repository_id,expires_ms);
PRAGMA user_version=7;
