package gitinfo

import (
	"errors"
	"testing"
)

// Measured on NTFS: each pair below reached one file, and the hook let the second spelling
// through because the path did not match the worktree root or the claimed name.
func TestRelativeFollowsWindowsNames(t *testing.T) {
	for _, c := range []struct{ root, path, want string }{
		{`C:\Users\a\repo`, `C:\USERS\A\REPO\src\x.go`, "src/x.go"},
		{`C:\Users\a\repo`, `c:/users/a/repo/src/x.go`, "src/x.go"},
		{`C:\repo`, `C:\repo\src\t.go.`, "src/t.go"},
		{`C:\repo`, `C:\repo\src\s.go `, "src/s.go"},
		{`C:\repo`, `C:\repo\d.\x.go`, "d/x.go"},
		{`C:\repo`, `C:\repo\d . \x.go. .`, "d/x.go"},
		{`C:\repo.`, `C:\repo\x.go`, "x.go"},
		{`C:\repo`, `src\t.go.`, "src/t.go"},
		{`C:\repo`, `C:\repo\...\x.go`, ".../x.go"},
	} {
		got, err := Relative(c.root, c.path)
		if err != nil || got != c.want {
			t.Errorf("Relative(%q, %q) = %q, %v; want %q", c.root, c.path, got, err, c.want)
		}
	}
}

func TestRelativeKeepsPOSIXNamesExact(t *testing.T) {
	if _, err := Relative("/home/a/Repo", "/home/a/repo/x.go"); !errors.Is(err, ErrEscape) {
		t.Fatalf("a case-sensitive root matched another case: %v", err)
	}
	if got, err := Relative("/repo", "/repo/t.go."); err != nil || got != "t.go." {
		t.Fatalf("a POSIX name lost its trailing dot: %q %v", got, err)
	}
	if _, err := Relative(`C:\repo`, `D:\repo\x.go`); !errors.Is(err, ErrEscape) {
		t.Fatalf("another drive matched: %v", err)
	}
	if _, err := Relative(`C:\repo`, `C:\repository\x.go`); !errors.Is(err, ErrEscape) {
		t.Fatalf("a longer name matched: %v", err)
	}
}
