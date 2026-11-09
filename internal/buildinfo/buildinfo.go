// Package buildinfo describes the source used to build the running Iris binary.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// These values may be set by the build with -ldflags. Empty Commit and Modified
// fall back to Go's embedded VCS metadata. Modified accepts true, false or unknown;
// an explicit unknown prevents generated build files from being called source dirt.
var (
	Version  = "dev"
	Commit   = ""
	Modified = ""
)

// String returns the same build identity for the CLI, hub and diagnostic bundle.
// Missing metadata stays unknown: an archive without .git is not proof of a clean tree.
// Source state does not verify the freshness of the ignored embedded console assets,
// even when the build supplies explicit linker metadata.
func String() string {
	info, _ := debug.ReadBuildInfo()
	return describe(Version, Commit, Modified, info)
}

func describe(version, commit, modified string, info *debug.BuildInfo) string {
	if version == "" {
		version = "dev"
	}
	if info != nil {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = setting.Value
				}
			case "vcs.modified":
				if modified == "" {
					modified = setting.Value
				}
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	state := "modified unknown"
	switch modified {
	case "true":
		state = "dirty"
	case "false":
		state = "clean"
	}
	return fmt.Sprintf("%s (commit %s; source %s; console unverified)", version, commit, state)
}
