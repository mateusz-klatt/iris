package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestUserServiceDoesNotNeedIrisOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	mgr, err := userService("127.0.0.1:1", filepath.Join(t.TempDir(), "hub.token"))
	if err != nil || mgr == nil {
		t.Fatalf("%v %v", mgr, err)
	}
}

func TestServiceBinary(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not on PATH") }
	self := func() (string, error) { return "/running/iris", nil }

	found := func(name string) (string, error) {
		if name != "iris" {
			t.Fatal(name)
		}
		return "iris-on-path", nil
	}
	want, err := filepath.Abs("iris-on-path")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := serviceBinary(found, self); got != want || err != nil {
		t.Fatalf("PATH wins: %q %v", got, err)
	}
	if got, err := serviceBinary(missing, self); got != "/running/iris" || err != nil {
		t.Fatalf("a logon without iris on PATH: %q %v", got, err)
	}
	broken := errors.New("no executable path")
	if _, err := serviceBinary(missing, func() (string, error) { return "", broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}
