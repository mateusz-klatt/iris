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
		{"no build metadata", "dev", "", "", nil, "dev (commit unknown; modified unknown)"},
		{"empty version", "", "", "", &debug.BuildInfo{}, "dev (commit unknown; modified unknown)"},
		{"clean go build", "dev", "", "", metadata("false"), "dev (commit " + revision + "; clean)"},
		{"dirty go build", "dev", "", "", metadata("true"), "dev (commit " + revision + "; dirty)"},
		{"unknown modification", "dev", "", "", metadata("future-value"), "dev (commit " + revision + "; modified unknown)"},
		{"release identity overrides generated tree", "v1.2.3", "release-revision", "false", metadata("true"), "v1.2.3 (commit release-revision; clean)"},
		{"container without git", "dev", revision, "true", nil, "dev (commit " + revision + "; dirty)"},
		{"commit override keeps embedded modification", "dev", "override", "", metadata("true"), "dev (commit override; dirty)"},
		{"modification override keeps embedded revision", "dev", "", "unknown", metadata("false"), "dev (commit " + revision + "; modified unknown)"},
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
	if got := String(); got != "v1.2.3 (commit fedcba9876543210; clean)" {
		t.Fatalf("build identity %q", got)
	}
}
