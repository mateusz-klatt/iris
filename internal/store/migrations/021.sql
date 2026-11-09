-- Idle maintenance visits live coordination state rather than historical rows.
CREATE INDEX IF NOT EXISTS execution_active_deadline ON Execution(deadline_tick_ms,recovery_deadline,boot_epoch) WHERE state='active';
CREATE INDEX IF NOT EXISTS claim_unrevoked_execution ON Claim(execution_id,expires_ms,repository_id) WHERE revoked_ms IS NULL;
CREATE INDEX IF NOT EXISTS delivery_attention_recipient ON Delivery(recipient_agent_id) WHERE attention=1;
CREATE INDEX IF NOT EXISTS event_recipient_cursor ON Event(agent_id,seq);
CREATE INDEX IF NOT EXISTS console_session_expiry ON ConsoleSession(expires_ms);
-- Legacy handover settlements could leave an already-acknowledged fetch due.
-- This cleanup and index creation are one-time startup work, not idle sweeps.
UPDATE Delivery SET receipt_due_ms=NULL WHERE receipt_due_ms IS NOT NULL
 AND EXISTS(SELECT 1 FROM Ack WHERE delivery_id=Delivery.id);
PRAGMA user_version=21;
