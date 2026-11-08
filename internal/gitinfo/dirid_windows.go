package gitinfo

// directoryID is not measured on Windows, where a worktree reached by two paths, through a
// junction or a mapped drive, still counts as two (D6). It is always "", and the hub then keys
// the worktree on its path, as before.
func directoryID(string) string { return "" }
