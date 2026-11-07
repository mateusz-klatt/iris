package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "11d5960a326750d5838078e36cf38b85af677262"

func TestCheck(t *testing.T) {
	text := strings.Join([]string{
		"steps:",
		"  - uses: actions/checkout@" + sha + " # v4",
		"  - uses: actions/setup-go@v5",
		"    uses: 'owner/repo/path@main'",
		"  - uses: ./local-action",
		"  - uses: docker://alpine:3",
		"  - run: echo uses: nothing@v1",
		"  - uses: owner/repo@" + strings.ToUpper(sha),
	}, "\n")
	got := check("ci.yml", text)
	want := []string{
		"ci.yml:3: actions/setup-go@v5 is not pinned to a full commit SHA",
		"ci.yml:4: owner/repo/path@main is not pinned to a full commit SHA",
		"ci.yml:8: owner/repo@" + strings.ToUpper(sha) + " is not pinned to a full commit SHA",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q", got)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yml", "- uses: actions/checkout@"+sha+"\n")
	var out bytes.Buffer
	if code := run([]string{dir}, &out); code != 0 || out.Len() != 0 {
		t.Fatal(code, out.String())
	}
	write(t, dir, "b.yaml", "- uses: actions/checkout@v4\n")
	out.Reset()
	if code := run([]string{dir}, &out); code != 1 || !strings.Contains(out.String(), "b.yaml:1") {
		t.Fatal(code, out.String())
	}
}

func TestRunRefusals(t *testing.T) {
	var out bytes.Buffer
	if code := run(nil, &out); code != 2 {
		t.Fatal(code)
	}
	if code := run([]string{t.TempDir()}, &out); code != 2 || !strings.Contains(out.String(), "no workflows") {
		t.Fatal(code, out.String())
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "x.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{dir}, &out); code != 2 {
		t.Fatal(code)
	}
}
