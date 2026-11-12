//go:build !windows

package hook

import "os"

// protectSessionsLock normalizes permissions on the open file rather than a path another
// process could replace. Keeping the OS call injectable also exercises refusal and close.
var protectSessionsLock = func(f *os.File) error { return f.Chmod(0o600) }
