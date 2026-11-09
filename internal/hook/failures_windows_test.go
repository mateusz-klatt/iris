package hook

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFailureLogPreservesAFileHeldWithoutSharing(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := appendFailure(token, Failure{Stage: "session"}); err != nil {
		t.Fatal(err)
	}
	t.Run("held", func(t *testing.T) {
		name, err := windows.UTF16PtrFromString(failurePath(token))
		if err != nil {
			t.Fatal(err)
		}
		// Appending opens the log for reading and writing, which conflicts with this handle.
		handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		})
		if err := appendFailure(token, Failure{Stage: "touch"}); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("append must report the sharing conflict: %v", err)
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
		t.Fatalf("failed writes must preserve the existing log: %v %v", entries, err)
	}
}
