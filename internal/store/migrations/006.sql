ALTER TABLE Claim ADD COLUMN path_key TEXT NOT NULL DEFAULT '';
UPDATE Repository SET case_policy='folded';
PRAGMA user_version=6;
