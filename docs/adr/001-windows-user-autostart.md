# Windows per-user autostart

Status: accepted, 2026-11-06. Updates D14.

The first non-elevated Windows 11 setup measured access denied from Task Scheduler
ONLOGON registration. Use the Iris HKCU Run value instead, with the stable binary
running `service start`. Install also starts immediately. The detached hub logs beside
the database and records its pid and creation time for identity-checked stopping.
The database lock remains authoritative. HKCU Run needs no administrator but provides
no crash supervision; a user can inspect the log and start the service again.
