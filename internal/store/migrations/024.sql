-- ADR 020: account import is retired; account and audit history stay.
DROP TABLE ConsoleAssignmentImport;
DROP TABLE ConsoleImport;
PRAGMA user_version=24;
