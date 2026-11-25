ALTER TABLE Execution ADD COLUMN prior_execution_id TEXT;
ALTER TABLE Execution ADD COLUMN creation_source TEXT;
ALTER TABLE Execution ADD COLUMN creation_proof TEXT;
CREATE INDEX IF NOT EXISTS execution_prior ON Execution(prior_execution_id) WHERE prior_execution_id IS NOT NULL;
PRAGMA user_version = 29;
