package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository, two directories above this package.
const repoRoot = "../.."

// Each check passes the repository's real files and rejects every mutation of them, with the
// counts the inline Python reported: 82 for the release, 9 for the signing proof, 66 for macOS.
func TestTheRealWorkflowsPassAndEveryMutationIsRejected(t *testing.T) {
	t.Chdir(repoRoot)
	for _, c := range []struct{ check, want string }{
		{"release", "release.yml: real workflow and config passed; all 82 mutations rejected\n"},
		{"signing-proof", "signing-proof.yml: real workflow passed; all 9 mutations rejected\n"},
		{"macos-proof", "macos-signing-proof.yml: real workflow passed; all 66 mutations rejected\n"},
	} {
		var out bytes.Buffer
		if code := run([]string{c.check}, &out, nil); code != 0 || out.String() != c.want {
			t.Errorf("%s: exit %d: %q", c.check, code, out.String())
		}
	}
}

// Without its files a check fails and says which; a wrong or missing argument is a usage error.
func TestRunReportsFailuresAndUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, check := range []string{"release", "signing-proof", "macos-proof"} {
		var out bytes.Buffer
		if code := run([]string{check}, &out, nil); code != 1 || !strings.Contains(out.String(), "iris-workflowguard "+check+":") {
			t.Errorf("%s: exit %d: %s", check, code, out.String())
		}
	}
	for _, args := range [][]string{nil, {"release", "extra"}, {"other"}} {
		var out bytes.Buffer
		if code := run(args, &out, nil); code != 2 || out.String() != usage+"\n" {
			t.Errorf("%v: exit %d: %s", args, code, out.String())
		}
	}
	failing := func(io.Writer) error { return errors.New("no goreleaser") }
	var out bytes.Buffer
	if code := run([]string{"windows-hook"}, &out, failing); code != 1 || !strings.Contains(out.String(), "no goreleaser") {
		t.Errorf("windows-hook: exit %d: %s", code, out.String())
	}
}

// main exits with run's status.
func TestMainExitsWithRunsStatus(t *testing.T) {
	code := -1
	args := os.Args
	exitProcess, exercise = func(c int) { code = c }, func(io.Writer) error { return nil }
	t.Cleanup(func() { exitProcess, exercise, os.Args = os.Exit, defaultExercise, args })
	os.Args = []string{"iris-workflowguard", "windows-hook"}
	main()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// A guard must fail its own self-test when a mutation does not change the verdict or cannot be
// applied, and must fail on a real document its check rejects.
func TestVerifyFailsOnAcceptedOrImpossibleMutations(t *testing.T) {
	doc := map[string]any{"a": "b"}
	strict := func(d any) error {
		if at(d, "a") != "b" {
			return errors.New("changed")
		}
		return nil
	}
	if err := verify("f.yml", doc, strict, []mutation{{[]any{"a"}, "c"}}); err != nil {
		t.Fatalf("a rejected mutation failed: %v", err)
	}
	if err := verify("f.yml", doc, strict, []mutation{{[]any{"other"}, "c"}}); err == nil || !strings.Contains(err.Error(), "f.yml: the guard accepted the mutation at [other]") {
		t.Errorf("an accepted mutation passed: %v", err)
	}
	if err := verify("f.yml", doc, strict, []mutation{{[]any{"a", "deeper"}, "c"}}); err == nil {
		t.Error("an impossible mutation passed")
	}
	if err := verify("f.yml", map[string]any{"a": "x"}, strict, nil); err == nil || !strings.Contains(err.Error(), "f.yml: changed") {
		t.Errorf("a failing document passed: %v", err)
	}
	if verify("f.yml", doc, strict, []mutation{{[]any{"a"}, "c"}}) != nil || at(doc, "a") != "b" {
		t.Error("a mutation reached the real document")
	}
}

// copyTree copies the files the checks read into a scratch repository, so a test can change one.
func copyTree(t *testing.T, change func(name string, text string) string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{".goreleaser.yaml", ".github/workflows/release.yml", ".github/workflows/signing-proof.yml", ".github/workflows/macos-signing-proof.yml"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(change(name, string(raw))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A guard whose files load but whose check fails says which file and what failed; the signing
// proof also needs both named steps to build its mutations.
func TestGuardsNameTheFailingFile(t *testing.T) {
	broken := copyTree(t, func(name, text string) string {
		return strings.ReplaceAll(strings.ReplaceAll(text, "cancel-in-progress: false", "cancel-in-progress: true"), "Build the release helper", "Build something")
	})
	if _, err := guardRelease(broken); err == nil || !strings.HasPrefix(err.Error(), "release.yml: releases must queue") {
		t.Errorf("release: %v", err)
	}
	if _, err := guardMacOSProof(broken); err == nil || !strings.HasPrefix(err.Error(), "macos-signing-proof.yml: proofs must queue") {
		t.Errorf("macOS: %v", err)
	}
	if _, err := guardSigningProof(broken); err == nil || !strings.Contains(err.Error(), `no step named "Build the release helper"`) {
		t.Errorf("signing proof: %v", err)
	}
	unsigned := copyTree(t, func(name, text string) string { return strings.ReplaceAll(text, "Sign each binary once", "Sign") })
	if _, err := guardSigningProof(unsigned); err == nil || !strings.Contains(err.Error(), `no step named "Sign each binary once"`) {
		t.Errorf("signing proof without its signing step: %v", err)
	}
	direct := copyTree(t, func(name, text string) string {
		return strings.ReplaceAll(text, `sign-windows --arch=arm64`, `sign-windows --arch=amd64`)
	})
	if _, err := guardSigningProof(direct); err == nil || !strings.HasPrefix(err.Error(), "signing-proof.yml: expected two separate helper processes") {
		t.Errorf("signing proof with a changed run: %v", err)
	}
	for _, missing := range []string{".goreleaser.yaml", ".github/workflows/signing-proof.yml"} {
		root := copyTree(t, func(name, text string) string { return text })
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(missing))); err != nil {
			t.Fatal(err)
		}
		if _, err := guardRelease(root); err == nil {
			t.Errorf("release without %s passed", missing)
		}
	}
}
