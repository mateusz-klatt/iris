-- ADR 014, storage stage only. No account-mode route is enabled by this schema.
CREATE TABLE ConsoleAccount (
 id TEXT NOT NULL PRIMARY KEY,
 username TEXT NOT NULL,
 login_key TEXT NOT NULL UNIQUE,
 password_hash TEXT NOT NULL,
 role TEXT NOT NULL DEFAULT 'none' CHECK(role IN ('operator','member','none')),
 disabled INTEGER NOT NULL DEFAULT 1 CHECK(disabled IN (0,1)),
 display_name TEXT NOT NULL DEFAULT '',
 ui_locale TEXT NOT NULL DEFAULT '',
 correspondence_locale TEXT NOT NULL DEFAULT '',
 created_ms INTEGER NOT NULL,
 last_login_ms INTEGER,
 auth_version INTEGER NOT NULL DEFAULT 1 CHECK(auth_version>0),
 edit_revision INTEGER NOT NULL DEFAULT 1 CHECK(edit_revision>0),
 CHECK(role<>'none' OR disabled=1)
);
CREATE TABLE ConsoleRepositoryAccess (
 account_id TEXT NOT NULL REFERENCES ConsoleAccount(id),
 repository_id TEXT NOT NULL REFERENCES Repository(id),
 mode TEXT NOT NULL CHECK(mode IN ('ro','rw')),
 PRIMARY KEY(account_id,repository_id)
);
CREATE TABLE ConsoleSession (
 digest BLOB NOT NULL PRIMARY KEY CHECK(length(digest)=32),
 account_id TEXT NOT NULL REFERENCES ConsoleAccount(id),
 auth_version INTEGER NOT NULL CHECK(auth_version>0),
 created_ms INTEGER NOT NULL,
 expires_ms INTEGER NOT NULL CHECK(expires_ms>created_ms AND expires_ms<=created_ms+43200000)
);
CREATE INDEX console_session_account ON ConsoleSession(account_id);
CREATE TABLE ConsoleImport (
 source_id TEXT NOT NULL,
 source_row_id TEXT NOT NULL,
 account_id TEXT NOT NULL UNIQUE REFERENCES ConsoleAccount(id),
 fingerprint_version INTEGER NOT NULL CHECK(fingerprint_version=1),
 source_fingerprint BLOB NOT NULL CHECK(length(source_fingerprint)=32),
 created_ms INTEGER NOT NULL,
 PRIMARY KEY(source_id,source_row_id)
);
ALTER TABLE Message ADD COLUMN sender_account_id TEXT REFERENCES ConsoleAccount(id);
ALTER TABLE Message ADD COLUMN sender_username TEXT;
ALTER TABLE Message ADD COLUMN sender_display_name TEXT;
CREATE TRIGGER console_authorship_insert BEFORE INSERT ON Message
 WHEN NOT ((NEW.sender_account_id IS NULL AND NEW.sender_username IS NULL AND NEW.sender_display_name IS NULL)
 OR (NEW.sender_account_id IS NOT NULL AND NEW.sender_username IS NOT NULL AND NEW.sender_display_name IS NOT NULL))
 BEGIN SELECT RAISE(ABORT,'incomplete console authorship'); END;
CREATE TRIGGER console_authorship_immutable BEFORE UPDATE OF sender_account_id,sender_username,sender_display_name ON Message
 WHEN OLD.sender_account_id IS NOT NEW.sender_account_id OR OLD.sender_username IS NOT NEW.sender_username
 OR OLD.sender_display_name IS NOT NEW.sender_display_name
 BEGIN SELECT RAISE(ABORT,'console authorship is immutable'); END;
CREATE TABLE ConsoleOperation (
 account_id TEXT NOT NULL REFERENCES ConsoleAccount(id),
 key TEXT NOT NULL,
 request_hash BLOB NOT NULL CHECK(length(request_hash)=32),
 -- Preserve the accepted origin even after its repository id is retired by repair.
 origin_repository_id TEXT NOT NULL,
 message_id TEXT REFERENCES Message(id) ON DELETE SET NULL,
 result_json TEXT,
 recipient_ids_json TEXT NOT NULL CHECK(json_valid(recipient_ids_json) AND json_type(recipient_ids_json)='array'),
 created_ms INTEGER NOT NULL,
 tombstone INTEGER NOT NULL DEFAULT 0 CHECK(tombstone IN (0,1)),
 PRIMARY KEY(account_id,key)
);
CREATE INDEX console_operation_created ON ConsoleOperation(created_ms);
CREATE TRIGGER console_operation_message_delete BEFORE DELETE ON Message BEGIN
 UPDATE ConsoleOperation SET tombstone=1,result_json=NULL WHERE message_id=OLD.id;
END;
CREATE TRIGGER console_operation_delivery_delete BEFORE DELETE ON Delivery BEGIN
 UPDATE ConsoleOperation SET tombstone=1,result_json=NULL WHERE message_id=OLD.message_id;
END;
CREATE TRIGGER console_operation_recipient_delete BEFORE DELETE ON Agent BEGIN
 UPDATE ConsoleOperation SET tombstone=1,result_json=NULL
 WHERE EXISTS(SELECT 1 FROM json_each(recipient_ids_json) WHERE value=OLD.id);
END;
CREATE TABLE ConsoleAction (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 actor_account_id TEXT NOT NULL REFERENCES ConsoleAccount(id),
 target_account_id TEXT REFERENCES ConsoleAccount(id),
 action TEXT NOT NULL,
 outcome TEXT NOT NULL,
 created_ms INTEGER NOT NULL
);
PRAGMA user_version=17;
