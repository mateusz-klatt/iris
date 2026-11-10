-- A delivery keeps the cursor of the newest inbox_changed event committed for it, so a session's
-- attention identity (ADR 017, decision 4 as amended) outlives the event: retention deletes
-- events after seven days, or after an hour once every live cursor has passed them, and never
-- deletes mail.
ALTER TABLE Delivery ADD COLUMN attention_seq INTEGER;
-- The database puts the cursor on the delivery as the event is inserted, whichever binary inserts
-- it, so a hub from before this migration that runs on this database after a rollback keeps every
-- delivery's cursor current, as this one does. Every producer writes its delivery first.
CREATE TRIGGER delivery_attention_seq AFTER INSERT ON Event WHEN NEW.kind='inbox_changed' BEGIN
 UPDATE Delivery SET attention_seq=NEW.seq WHERE id=NEW.object_id;
END;
PRAGMA user_version=23;
