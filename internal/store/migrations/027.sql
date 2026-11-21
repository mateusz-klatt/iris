-- A full shell baseline from before ADR 033 has no recorded checkout. An older hook could leave
-- one taken in a submodule under a superproject Execution, and comparing the Execution's own
-- status with it could release that session's claims or take misnamed ones. Each is dropped once,
-- so the next shell command of each session sets a baseline before anything is attributed.
DELETE FROM ShellSnapshot;
PRAGMA user_version=27;
