-- ADR 014 authorization stage. Routes remain gated until the cutover step.
-- Console cursors count only changes in this account's current repository scope.
-- Event sequence numbers, global wakeups and coordination-only heartbeats never
-- become member cursors.
CREATE TABLE ConsoleRevision (
 account_id TEXT NOT NULL PRIMARY KEY REFERENCES ConsoleAccount(id),
 revision INTEGER NOT NULL DEFAULT 0 CHECK(revision>=0)
);
INSERT INTO ConsoleRevision(account_id) SELECT id FROM ConsoleAccount;
CREATE VIEW ConsoleRepositoryChange AS SELECT CAST(NULL AS TEXT) AS repository_id WHERE 0;
CREATE TRIGGER console_repository_change INSTEAD OF INSERT ON ConsoleRepositoryChange BEGIN
 UPDATE ConsoleRevision SET revision=revision+1 WHERE account_id IN (
  SELECT a.id FROM ConsoleAccount a WHERE a.disabled=0 AND
  (a.role='operator' OR (a.role='member' AND EXISTS (
   SELECT 1 FROM ConsoleRepositoryAccess g WHERE g.account_id=a.id
   AND g.repository_id=NEW.repository_id AND g.mode IN ('ro','rw')
  )))
 );
END;
CREATE TRIGGER console_revision_account_insert AFTER INSERT ON ConsoleAccount BEGIN
 INSERT INTO ConsoleRevision(account_id) VALUES(NEW.id);
 UPDATE ConsoleRevision SET revision=revision+1 WHERE account_id<>NEW.id AND account_id IN
 (SELECT id FROM ConsoleAccount WHERE disabled=0 AND role='operator');
END;
CREATE TRIGGER console_revision_account_update AFTER UPDATE ON ConsoleAccount BEGIN
 UPDATE ConsoleRevision SET revision=revision+1 WHERE account_id=NEW.id OR account_id IN
 (SELECT id FROM ConsoleAccount WHERE disabled=0 AND role='operator');
END;
CREATE TRIGGER console_revision_grant_insert AFTER INSERT ON ConsoleRepositoryAccess BEGIN
 UPDATE ConsoleRevision SET revision=revision+1 WHERE account_id=NEW.account_id OR account_id IN
 (SELECT id FROM ConsoleAccount WHERE disabled=0 AND role='operator');
END;
CREATE TRIGGER console_revision_grant_update AFTER UPDATE ON ConsoleRepositoryAccess BEGIN
 UPDATE ConsoleRevision SET revision=revision+1 WHERE account_id IN (OLD.account_id,NEW.account_id) OR account_id IN
 (SELECT id FROM ConsoleAccount WHERE disabled=0 AND role='operator');
END;
CREATE TRIGGER console_revision_grant_delete AFTER DELETE ON ConsoleRepositoryAccess BEGIN
 UPDATE ConsoleRevision SET revision=revision+1 WHERE account_id=OLD.account_id OR account_id IN
 (SELECT id FROM ConsoleAccount WHERE disabled=0 AND role='operator');
END;

CREATE TRIGGER console_revision_repository_insert AFTER INSERT ON Repository BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT NEW.id;
END;

CREATE TRIGGER console_revision_repository_update AFTER UPDATE ON Repository BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.id UNION SELECT NEW.id;
END;

CREATE TRIGGER console_revision_repository_delete AFTER DELETE ON Repository BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.id;
END;

CREATE TRIGGER console_revision_repositoryalias_insert AFTER INSERT ON RepositoryAlias BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_repositoryalias_update AFTER UPDATE ON RepositoryAlias BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_repositoryalias_delete AFTER DELETE ON RepositoryAlias BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id;
END;

CREATE TRIGGER console_revision_agent_insert AFTER INSERT ON Agent BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_agent_update AFTER UPDATE ON Agent BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_agent_delete AFTER DELETE ON Agent BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id;
END;

CREATE TRIGGER console_revision_execution_insert AFTER INSERT ON Execution BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT NEW.repository_id;
END;

-- last_seen_ms is visible only to this repository's readers. Lease renewal and
-- watch-seen fields alone do not change the console projection or its cursor.
CREATE TRIGGER console_revision_execution_update AFTER UPDATE OF repository_id,branch,state,last_seen_ms ON Execution BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_execution_delete AFTER DELETE ON Execution BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id;
END;

CREATE TRIGGER console_revision_claim_insert AFTER INSERT ON Claim BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_claim_update AFTER UPDATE ON Claim BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_claim_delete AFTER DELETE ON Claim BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id;
END;

CREATE TRIGGER console_revision_message_insert AFTER INSERT ON Message BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_message_update AFTER UPDATE ON Message BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;

CREATE TRIGGER console_revision_message_delete AFTER DELETE ON Message BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id;
END;

CREATE TRIGGER console_revision_delivery_insert AFTER INSERT ON Delivery BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT repository_id FROM Agent WHERE id=NEW.recipient_agent_id;
END;

CREATE TRIGGER console_revision_delivery_update AFTER UPDATE ON Delivery BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT repository_id FROM Agent WHERE id=OLD.recipient_agent_id UNION SELECT repository_id FROM Agent WHERE id=NEW.recipient_agent_id;
END;

CREATE TRIGGER console_revision_delivery_delete AFTER DELETE ON Delivery BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT repository_id FROM Agent WHERE id=OLD.recipient_agent_id;
END;

CREATE TRIGGER console_revision_ack_insert AFTER INSERT ON Ack BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT a.repository_id FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.id=NEW.delivery_id;
END;

CREATE TRIGGER console_revision_ack_update AFTER UPDATE ON Ack BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT a.repository_id FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.id=OLD.delivery_id UNION SELECT a.repository_id FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.id=NEW.delivery_id;
END;

CREATE TRIGGER console_revision_ack_delete AFTER DELETE ON Ack BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT a.repository_id FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE d.id=OLD.delivery_id;
END;

CREATE TRIGGER console_revision_hostreport_insert AFTER INSERT ON HostReport BEGIN
 INSERT INTO ConsoleRepositoryChange VALUES(NULL);
END;

CREATE TRIGGER console_revision_hostreport_update AFTER UPDATE ON HostReport BEGIN
 INSERT INTO ConsoleRepositoryChange VALUES(NULL);
END;

CREATE TRIGGER console_revision_hostreport_delete AFTER DELETE ON HostReport BEGIN
 INSERT INTO ConsoleRepositoryChange VALUES(NULL);
END;

PRAGMA user_version=18;
