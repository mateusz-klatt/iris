-- Receipts (ADR 005). handled_ms is when the recipient said it handled the message: its
-- word, not a verified fact. DeliveryState gains the handled state, which the operator's
-- thread view and the sender's status show.
ALTER TABLE Ack ADD COLUMN handled_ms INTEGER;
DROP VIEW IF EXISTS DeliveryState;
CREATE VIEW DeliveryState AS SELECT d.*,CASE WHEN a.handled_ms IS NOT NULL THEN 'handled'
 WHEN a.delivery_id IS NOT NULL THEN 'acked'
 WHEN d.execution_id IS NULL THEN 'pending' ELSE 'inflight' END AS state,
 a.acked_ms,a.handled_ms
 FROM Delivery d LEFT JOIN Ack a ON a.delivery_id=d.id;
PRAGMA user_version=11;
