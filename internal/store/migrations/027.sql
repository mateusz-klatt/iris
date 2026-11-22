-- Pure bookkeeping updates on Claim and Delivery no longer move console cursors.
-- Lease renewals (Claim.expires_ms) and case-folding key updates (Claim.path_key) do
-- not change the console's live claim list unless execution_id, repository_id, kind,
-- path, origin or revoked_ms changes. The console renders expires_at as a live countdown
-- from the last overview refetch, so renewed auto claims keep counting down until the
-- next real change; release, revoke and expiry still write revoked_ms and move it.
-- On Delivery, attempt, attention, attention_seq and receipt_due_ms are internal
-- fetch/wake bookkeeping and are never shown by the console; recipient_agent_id and
-- execution_id (pending vs inflight), plus inserts, deletes and Ack changes, still
-- move the cursor. Each WHEN clause also skips same-value updates.
DROP TRIGGER console_revision_claim_update;
CREATE TRIGGER console_revision_claim_update AFTER UPDATE OF execution_id,repository_id,kind,path,origin,revoked_ms ON Claim
WHEN OLD.execution_id IS NOT NEW.execution_id OR OLD.repository_id IS NOT NEW.repository_id OR OLD.kind IS NOT NEW.kind OR OLD.path IS NOT NEW.path OR OLD.origin IS NOT NEW.origin OR OLD.revoked_ms IS NOT NEW.revoked_ms BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;
DROP TRIGGER console_revision_delivery_update;
CREATE TRIGGER console_revision_delivery_update AFTER UPDATE OF recipient_agent_id,execution_id ON Delivery
WHEN OLD.recipient_agent_id IS NOT NEW.recipient_agent_id OR OLD.execution_id IS NOT NEW.execution_id BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT repository_id FROM Agent WHERE id=OLD.recipient_agent_id UNION SELECT repository_id FROM Agent WHERE id=NEW.recipient_agent_id;
END;
PRAGMA user_version=27;
