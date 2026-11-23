-- A claim_available event records the repository that owned the wait. Replay
-- reads that repository's claims and policy. An older event leaves the column
-- null. Replay does not treat it as free: the Execution's repository is not a
-- substitute when the wait was for a submodule, and a wake could meet a claim
-- the replay did not see (ADR 033). A missed replay is the conservative failure.
ALTER TABLE Event ADD COLUMN repository_id TEXT;
PRAGMA user_version=30;
