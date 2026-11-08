ALTER TABLE Message ADD COLUMN repository_id TEXT REFERENCES Repository(id);
ALTER TABLE Message ADD COLUMN thread_id TEXT;
ALTER TABLE Message ADD COLUMN reply_to TEXT;
UPDATE Message SET repository_id=(SELECT repository_id FROM Execution WHERE id=sender_execution_id),thread_id=id
WHERE repository_id IS NULL;
ALTER TABLE Execution ADD COLUMN last_seen_ms INTEGER NOT NULL DEFAULT 0;
CREATE INDEX message_thread ON Message(thread_id,created_ms);
CREATE VIRTUAL TABLE MessageSearch USING fts5(subject,body,content='Message',content_rowid='rowid');
INSERT INTO MessageSearch(MessageSearch) VALUES('rebuild');
CREATE TRIGGER message_search_insert AFTER INSERT ON Message BEGIN
 INSERT INTO MessageSearch(rowid,subject,body) VALUES(new.rowid,new.subject,new.body);
END;
CREATE TABLE DoctorReport(id INTEGER PRIMARY KEY CHECK(id=1),report TEXT NOT NULL);
PRAGMA user_version=2;
