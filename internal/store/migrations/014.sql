-- One doctor report per host (ADR 007). The single report kept until now came from a doctor
-- that named no host, so it moves to the host "unknown", where no real host's report goes.
CREATE TABLE HostReport (host TEXT PRIMARY KEY, report TEXT NOT NULL);
INSERT INTO HostReport SELECT 'unknown',report FROM DoctorReport;
DROP TABLE DoctorReport;
PRAGMA user_version=14;
