// Command iris-workflowguard checks the release and proof workflows: that each runs only where and
// when it should, uses only reviewed actions, and keeps every signing secret inside the one step
// that needs it. Each check first passes the real files, then rejects every mutation in its list,
// so a check that stopped noticing a weakened workflow fails too.
//
// These checks were inline Python in ci.yml. Their text named secrets the way a workflow that
// leaks them would, and from 09:24 UTC on 13 November 2026 GitHub held ci.yml's runs, on main and
// on pull requests, as a workflow that "may be malicious" until someone with write access
// approved each one. Here they are ordinary Go with tests. The command lives under cmd/, not
// .github/, because it parses YAML: the go command skips directories whose names start with a
// dot, so `go mod tidy` would drop the YAML module from go.mod if only such a package used it.
//
// windows-hook runs the real goreleaser hook with a synthetic signer, which was the fourth inline
// script.
//
// Usage, from the repository root: go run ./cmd/iris-workflowguard release|signing-proof|macos-proof|windows-hook
package main

import (
	"fmt"
	"io"
	"os"
)

// exitProcess is the process boundary. Tests replace it so main can be called.
var exitProcess = os.Exit

// exercise runs the Windows hook. Tests replace it, since the real one builds with go and runs
// goreleaser.
var exercise = defaultExercise

func main() { exitProcess(run(os.Args[1:], os.Stdout, exercise)) }

const usage = "usage: iris-workflowguard release|signing-proof|macos-proof|windows-hook"

// run checks what args names in the repository at the working directory and reports on out.
// exercise runs the Windows hook; tests pass a stand-in.
func run(args []string, out io.Writer, exercise func(io.Writer) error) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(out, usage)
		return 2
	}
	var summary []string
	var err error
	switch args[0] {
	case "release":
		summary, err = guardRelease(".")
	case "signing-proof":
		summary, err = guardSigningProof(".")
	case "macos-proof":
		summary, err = guardMacOSProof(".")
	case "windows-hook":
		err = exercise(out)
	default:
		_, _ = fmt.Fprintln(out, usage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "iris-workflowguard %s: %v\n", args[0], err)
		return 1
	}
	for _, line := range summary {
		_, _ = fmt.Fprintln(out, line)
	}
	return 0
}

// guard collects a check's first failure; the later conditions still run, on values that may be
// nil, but only the first failure is reported, as the first failed assertion was.
type guard struct{ err error }

func (g *guard) need(ok bool, format string, args ...any) {
	if g.err == nil && !ok {
		g.err = fmt.Errorf(format, args...)
	}
}

// one is the single element of found, or nil after recording that there was not exactly one.
func (g *guard) one(found []any, what string) any {
	g.need(len(found) == 1, "expected exactly one %s, found %d", what, len(found))
	if len(found) != 1 {
		return nil
	}
	return found[0]
}

// A mutation is one weakening a check must reject: the value at path is replaced with value.
type mutation struct {
	path  []any
	value any
}

// verify checks the real document, then every mutation of it, and names the file in a failure.
func verify(file string, doc any, check func(any) error, mutations []mutation) error {
	if err := check(doc); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if err := rejectsAll(doc, mutations, check); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// rejectsAll runs check on a fresh copy of doc for each mutation. Every copy must fail; one that
// passes means the check no longer notices that weakening.
func rejectsAll(doc any, mutations []mutation, check func(any) error) error {
	for _, m := range mutations {
		mutated := clone(doc)
		if err := set(mutated, m.path, m.value); err != nil {
			return err
		}
		if check(mutated) == nil {
			return fmt.Errorf("the guard accepted the mutation at %v", m.path)
		}
	}
	return nil
}
