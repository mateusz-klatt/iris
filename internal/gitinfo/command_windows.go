package gitinfo

import "os/exec"

// stopDescendants does nothing on Windows: the deadline kills the process Go started, and what
// that process started can outlive it. That has not been measured there.
func stopDescendants(*exec.Cmd) {}
