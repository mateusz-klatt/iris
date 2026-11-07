package hubclient

import "os"

// privateDir creates the directory. On Windows the user's profile ACL protects it, and the
// token file inside carries its own owner-only DACL.
func privateDir(dir string) error { return os.MkdirAll(dir, 0o700) }
