package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// denyFileAccess changes only the test's temporary path and restores its DACL before cleanup.
func denyFileAccess(t *testing.T, path string, access uint32) {
	t.Helper()
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := before.DACL()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(D;;0x" + strconv.FormatUint(uint64(access), 16) + ";;;" + user.User.Sid.String() + ")(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, original, nil); err != nil {
			t.Error(err)
		}
	})
}

func TestFailureLogCannotInspectDeniedWindowsAttributes(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := appendFailure(token, Failure{Stage: "session"}); err != nil {
		t.Fatal(err)
	}
	t.Run("denied", func(t *testing.T) {
		denyFileAccess(t, failurePath(token), windows.FILE_READ_ATTRIBUTES)
		denyFileAccess(t, filepath.Dir(token), windows.FILE_LIST_DIRECTORY)
		if _, err := os.Lstat(failurePath(token)); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("the filesystem must refuse log inspection: %v", err)
		}
		if err := appendFailure(token, Failure{Stage: "touch"}); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("append must report denied log inspection: %v", err)
		}
		if _, err := ReadRecentFailures(token, 1); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("read must report denied log inspection: %v", err)
		}
		out := (Runner{ClientName: "kimi", Stdin: strings.NewReader("{"), Getenv: func(key string) string {
			if key == "IRIS_TOKEN_FILE" {
				return token
			}
			return ""
		}, after: never, logAfter: never}).Run()
		assertAllow(t, ClientKimi, out)
	})
	if entries, err := ReadRecentFailures(token, 10); err != nil || len(entries) != 1 || entries[0].Stage != "session" {
		t.Fatalf("denied writes must preserve the existing log: %v %v", entries, err)
	}
}
