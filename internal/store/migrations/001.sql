CREATE TABLE IF NOT EXISTS Meta (
 id INTEGER PRIMARY KEY CHECK(id=1), stream_id TEXT NOT NULL, boot_epoch INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS Repository (
 id TEXT PRIMARY KEY, case_policy TEXT NOT NULL DEFAULT 'sensitive' CHECK(case_policy IN ('sensitive','folded')),
 root_commit TEXT NOT NULL, conflict_action TEXT NOT NULL DEFAULT 'deny' CHECK(conflict_action IN ('deny','ask','warn'))
);
CREATE TABLE IF NOT EXISTS RepositoryAlias (
 alias TEXT NOT NULL, repository_id TEXT NOT NULL REFERENCES Repository(id), PRIMARY KEY(alias,repository_id)
);
CREATE TABLE IF NOT EXISTS Agent (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, repository_id TEXT NOT NULL REFERENCES Repository(id),
 host TEXT NOT NULL, client TEXT NOT NULL, worktree TEXT NOT NULL, slot INTEGER NOT NULL,
 UNIQUE(repository_id,host,client,worktree,slot)
);
CREATE TABLE IF NOT EXISTS Execution (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES Agent(id), repository_id TEXT NOT NULL REFERENCES Repository(id),
 host_id TEXT NOT NULL, client TEXT NOT NULL, session_id TEXT NOT NULL, generation INTEGER NOT NULL CHECK(generation>0),
 worktree TEXT NOT NULL, branch TEXT NOT NULL, boot_epoch INTEGER NOT NULL, deadline_tick_ms INTEGER NOT NULL,
 recovery_deadline INTEGER, state TEXT NOT NULL CHECK(state IN ('active','ended','lost')),
 parent_id TEXT REFERENCES Execution(id), client_pid INTEGER NOT NULL, client_pid_start_ms INTEGER NOT NULL,
 single_session INTEGER NOT NULL, heartbeat_mode INTEGER NOT NULL DEFAULT 0,
 UNIQUE(host_id,client,session_id,generation)
);
CREATE UNIQUE INDEX IF NOT EXISTS execution_live ON Execution(host_id,client,session_id) WHERE state='active';
CREATE TABLE IF NOT EXISTS Claim (
 fence INTEGER PRIMARY KEY AUTOINCREMENT, execution_id TEXT NOT NULL REFERENCES Execution(id),
 repository_id TEXT NOT NULL REFERENCES Repository(id), kind TEXT NOT NULL CHECK(kind IN ('exact','prefix')),
 path TEXT NOT NULL, origin TEXT NOT NULL CHECK(origin IN ('explicit','auto')), boot_epoch INTEGER NOT NULL,
 expires_ms INTEGER, revoked_ms INTEGER, CHECK(kind='prefix' OR path<>'')
);
CREATE INDEX IF NOT EXISTS claim_scan ON Claim(repository_id,revoked_ms);
CREATE TABLE IF NOT EXISTS Message (
 id TEXT PRIMARY KEY, sender_execution_id TEXT NOT NULL REFERENCES Execution(id), subject TEXT NOT NULL,
 body TEXT NOT NULL CHECK(length(CAST(body AS BLOB))<=16384), created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS Delivery (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES Message(id), recipient_agent_id TEXT NOT NULL REFERENCES Agent(id),
 execution_id TEXT REFERENCES Execution(id), attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt>=0),
 UNIQUE(message_id,recipient_agent_id)
);
CREATE TABLE IF NOT EXISTS Ack (
 delivery_id TEXT PRIMARY KEY REFERENCES Delivery(id), execution_id TEXT NOT NULL REFERENCES Execution(id),
 attempt INTEGER NOT NULL CHECK(attempt>0), acked_ms INTEGER NOT NULL
);
CREATE VIEW IF NOT EXISTS DeliveryState AS SELECT d.*,CASE WHEN a.delivery_id IS NOT NULL THEN 'acked'
 WHEN d.execution_id IS NULL THEN 'pending' ELSE 'inflight' END AS state
 FROM Delivery d LEFT JOIN Ack a ON a.delivery_id=d.id;
CREATE TABLE IF NOT EXISTS Event (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, agent_id TEXT REFERENCES Agent(id),
 execution_id TEXT REFERENCES Execution(id), object_id TEXT NOT NULL, created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS Cursor (
 consumer_id TEXT PRIMARY KEY, execution_id TEXT NOT NULL REFERENCES Execution(id), seq INTEGER NOT NULL CHECK(seq>=0)
);
CREATE TABLE IF NOT EXISTS Operation (
 principal_agent_id TEXT NOT NULL REFERENCES Agent(id), key TEXT NOT NULL, request_hash BLOB NOT NULL,
 result_json TEXT NOT NULL, PRIMARY KEY(principal_agent_id,key)
);
