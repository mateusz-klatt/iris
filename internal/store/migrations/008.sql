-- attention marks a delivery whose recipient still needs a wake. It is separate from attempt,
-- which only guards acknowledgements, and from execution_id, which says who is processing it.
-- A fetch clears it. Losing the fetching Execution sets it again, but a hub restart does not.
ALTER TABLE Delivery ADD COLUMN attention INTEGER NOT NULL DEFAULT 0;
UPDATE Delivery SET attention=1 WHERE attempt=0 AND id NOT IN (SELECT delivery_id FROM Ack);
-- requeue runs on every expire and looks deliveries up by Execution.
CREATE INDEX IF NOT EXISTS delivery_execution ON Delivery(execution_id);
PRAGMA user_version=8;
