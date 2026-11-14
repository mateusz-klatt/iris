//go:build !linux

package hook

import "testing"

func TestOtherSystemsCannotReadCursorArguments(t *testing.T) {
	if _, err := unreadCursorArgs(1, 0); err == nil {
		t.Fatal("a non-Linux reader returned arguments")
	}
}
