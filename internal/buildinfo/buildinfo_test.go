package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestDescribeUsesAvailableSourceIdentityWithoutGuessing(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	metadata := func(modified string) *debug.BuildInfo {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "GOARCH", Value: "amd64"},
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.modified", Value: modified},
		}}
	}
	for _, tc := range []struct {
		name, version, commit, modified string
		info                            *debug.BuildInfo
		want                            string
	}{
		{"no build metadata", "dev", "", "", nil, "dev (commit unknown; source modified unknown; console unverified)"},
		{"empty version", "", "", "", &debug.BuildInfo{}, "dev (commit unknown; source modified unknown; console unverified)"},
		{"clean source does not verify console", "dev", "", "", metadata("false"), "dev (commit " + revision + "; source clean; console unverified)"},
		{"dirty go build", "dev", "", "", metadata("true"), "dev (commit " + revision + "; source dirty; console unverified)"},
		{"unknown modification", "dev", "", "", metadata("future-value"), "dev (commit " + revision + "; source modified unknown; console unverified)"},
		{"release identity overrides generated tree without verifying console", "v1.2.3", "release-revision", "false", metadata("true"), "v1.2.3 (commit release-revision; source clean; console unverified)"},
		{"container without git", "dev", revision, "true", nil, "dev (commit " + revision + "; source dirty; console unverified)"},
		{"clean container without git does not verify console", "v1.2.3", revision, "false", nil, "v1.2.3 (commit " + revision + "; source clean; console unverified)"},
		{"commit override keeps embedded modification", "dev", "override", "", metadata("true"), "dev (commit override; source dirty; console unverified)"},
		{"modification override keeps embedded revision", "dev", "", "unknown", metadata("false"), "dev (commit " + revision + "; source modified unknown; console unverified)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describe(tc.version, tc.commit, tc.modified, tc.info); got != tc.want {
				t.Fatalf("build identity %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStringUsesTheLinkerFieldsForEveryConsumer(t *testing.T) {
	oldVersion, oldCommit, oldModified := Version, Commit, Modified
	t.Cleanup(func() { Version, Commit, Modified = oldVersion, oldCommit, oldModified })
	Version, Commit, Modified = "v1.2.3", "fedcba9876543210", "false"
	if got := String(); got != "v1.2.3 (commit fedcba9876543210; source clean; console unverified)" {
		t.Fatalf("build identity %q", got)
	}
}
