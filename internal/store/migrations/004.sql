CREATE TABLE ShellSnapshot (
 execution_id TEXT PRIMARY KEY REFERENCES Execution(id),
 status_json TEXT NOT NULL
);
PRAGMA user_version=4;
