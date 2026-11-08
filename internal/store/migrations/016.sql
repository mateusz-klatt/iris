-- When a fetch that asked for a receipt is due one (ADR 010). Null when none is awaited. A fetch
-- with this set leaves the delivery's attention on until its receipt clears both.
ALTER TABLE Delivery ADD COLUMN receipt_due_ms INTEGER;
CREATE INDEX DeliveryReceiptDue ON Delivery(receipt_due_ms) WHERE receipt_due_ms IS NOT NULL;
PRAGMA user_version=16;
