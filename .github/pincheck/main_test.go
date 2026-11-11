package main

import (
	"bytes"
	"os"
	"os/exec"
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

func TestSecretsScanScope(t *testing.T) {
	root := filepath.Join("..", "..")
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, job, found := strings.Cut(strings.ReplaceAll(string(ci), "\r\n", "\n"), "  check-all:\n")
	if !found {
		t.Fatal("CI has no check-all job")
	}
	job, _, _ = strings.Cut(job, "  goreleaser-check:\n")
	for _, want := range []string{
		"fetch-depth: 0",
		"- run: make check-all\n        env:\n          GITLEAKS_LOG_OPTS: ${{ github.event_name == 'pull_request' && format('{0}..{1}', github.event.pull_request.base.sha, github.event.pull_request.head.sha) || '--all' }}",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("CI check-all must retain %q", want)
		}
	}
	for _, tc := range []struct{ name, override, want string }{
		{"local history", "", "HEAD"},
		{"pull request", "base..head", "base..head"},
		{"main full history", "--all", "--all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--no-print-directory", "--dry-run", "secrets", "GO=go"}
			if tc.override != "" {
				args = append(args, "GITLEAKS_LOG_OPTS="+tc.override)
			}
			cmd := exec.Command("make", args...)
			cmd.Dir = root
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "GITLEAKS_LOG_OPTS=") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make secrets dry-run: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), ` git --redact --no-banner --log-opts="`+tc.want+`" .`) {
				t.Fatalf("secrets command did not select %q: %s", tc.want, out)
			}
		})
	}
}
