PRAGMA foreign_keys=ON;
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
CREATE TABLE Meta (
 id INTEGER PRIMARY KEY CHECK(id=1), stream_id TEXT NOT NULL,
 boot_epoch INTEGER NOT NULL CHECK(boot_epoch>0)
);
CREATE TABLE Repository (
 id TEXT PRIMARY KEY, case_policy TEXT NOT NULL
 CHECK(case_policy IN ('sensitive','folded'))
);
CREATE TABLE Agent (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE Execution (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES Agent(id),
 repository_id TEXT NOT NULL REFERENCES Repository(id),
 host_id TEXT NOT NULL, client TEXT NOT NULL, session_id TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation>0),
 worktree TEXT NOT NULL, branch TEXT,
 boot_epoch INTEGER NOT NULL, deadline_tick_ms INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','ended','lost')),
 UNIQUE(host_id,client,session_id,generation)
);
CREATE UNIQUE INDEX execution_live ON Execution(host_id,client,session_id)
 WHERE state='active';
CREATE TABLE Claim (
 fence INTEGER PRIMARY KEY AUTOINCREMENT,
 execution_id TEXT NOT NULL REFERENCES Execution(id),
 repository_id TEXT NOT NULL REFERENCES Repository(id),
 kind TEXT NOT NULL CHECK(kind IN ('exact','prefix')),
 path TEXT NOT NULL, boot_epoch INTEGER NOT NULL,
 deadline_tick_ms INTEGER NOT NULL, revoked_ms INTEGER,
 CHECK(kind='prefix' OR path<>'')
);
CREATE INDEX claim_scan ON Claim(repository_id,revoked_ms,deadline_tick_ms);
CREATE TABLE Message (
 id TEXT PRIMARY KEY, sender_execution_id TEXT NOT NULL REFERENCES Execution(id),
 recipient_agent_id TEXT NOT NULL REFERENCES Agent(id),
 body BLOB NOT NULL CHECK(length(CAST(body AS BLOB))<=16384),
 created_ms INTEGER NOT NULL
);
CREATE TABLE Delivery (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL UNIQUE REFERENCES Message(id),
 execution_id TEXT REFERENCES Execution(id),
 attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt>=0),
 retry_tick_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE Ack (
 delivery_id TEXT PRIMARY KEY REFERENCES Delivery(id),
 execution_id TEXT NOT NULL REFERENCES Execution(id),
 attempt INTEGER NOT NULL CHECK(attempt>0), acked_ms INTEGER NOT NULL
);
CREATE VIEW DeliveryState AS
 SELECT d.*,CASE WHEN a.delivery_id IS NOT NULL THEN 'acked'
 WHEN d.execution_id IS NULL THEN 'pending' ELSE 'inflight' END AS state
 FROM Delivery d LEFT JOIN Ack a ON a.delivery_id=d.id;
CREATE TABLE Event (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL,
 agent_id TEXT REFERENCES Agent(id), execution_id TEXT REFERENCES Execution(id),
 object_id TEXT NOT NULL, created_ms INTEGER NOT NULL
);
CREATE TABLE Cursor (
 consumer_id TEXT PRIMARY KEY, execution_id TEXT NOT NULL REFERENCES Execution(id),
 seq INTEGER NOT NULL CHECK(seq>=0)
);
CREATE TABLE Operation (
 principal_agent_id TEXT NOT NULL REFERENCES Agent(id), key TEXT NOT NULL,
 request_hash BLOB NOT NULL, result_json TEXT NOT NULL,
 PRIMARY KEY(principal_agent_id,key)
);
