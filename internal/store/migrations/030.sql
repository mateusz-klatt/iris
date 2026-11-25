CREATE INDEX IF NOT EXISTS execution_process_hook ON Execution(host_id, client, client_pid, client_pid_start_ms) WHERE hook_seen_ms > 0;
PRAGMA user_version = 30;
