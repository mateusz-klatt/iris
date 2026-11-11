package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchAndMain(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"sign-windows", "--bad"}, {"sign-windows", "extra"}, {"sign-windows"}, {"verify-windows-archives", "--bad"}, {"verify-windows-archives", "extra"}, {"verify-windows-archives"}} {
		var out, stderr bytes.Buffer
		if got := dispatch(args, func(string) string { return "" }, &out, &stderr); got != 1 || out.Len() != 0 || !strings.HasPrefix(stderr.String(), "release tools: ") {
			t.Fatalf("dispatch %v: status %d, stdout %q, stderr %q", args, got, out.String(), stderr.String())
		}
	}
	oldExit, oldArgs, oldErr := exitProcess, os.Args, os.Stderr
	t.Cleanup(func() { exitProcess, os.Args, os.Stderr = oldExit, oldArgs, oldErr })
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	os.Args, os.Stderr = []string{"iris-release-tools", "unknown"}, file
	status := -1
	exitProcess = func(code int) { status = code }
	main()
	if status != 1 {
		t.Fatalf("main status: %d", status)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "release tools: unknown command\n" {
		t.Fatalf("main stderr %q: %v", data, err)
	}
}
