//go:build windows

package hook

import "os"

// Windows inherits the sessions directory's owner-only DACL and has no Unix umask to repair.
var protectSessionsLock = func(*os.File) error { return nil }
