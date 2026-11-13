// Command covermerge joins Go cover profiles from several operating systems, so
// that _windows.go and _darwin.go files count towards the coverage gate.
//
// A block counts as covered if any profile covered it. Blocks are matched by file
// and position, which are the same on every system built from one commit.
//
// With -check it writes no profile: it lists every statement left uncovered and fails
// if there is one. It counts statements because a percentage rounds 10306 of 10309 to
// 100.0%.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("covermerge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	check := flags.Bool("check", false, "list uncovered statements and fail if there are any")
	if err := flags.Parse(args); err != nil || flags.NArg() == 0 {
		return errors.New("usage: covermerge [-check] profile [profile ...]")
	}
	inputs := make([]io.Reader, 0, flags.NArg())
	for _, path := range flags.Args() {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		// Only read: closing cannot lose anything.
		defer func() { _ = f.Close() }()
		inputs = append(inputs, f)
	}
	if *check {
		return checkCovered(inputs, out)
	}
	return merge(inputs, out)
}

type block struct {
	statements int
	covered    bool
}

func load(inputs []io.Reader) (map[string]block, error) {
	blocks := map[string]block{}
	for _, in := range inputs {
		if err := read(in, blocks); err != nil {
			return nil, err
		}
	}
	return blocks, nil
}

func merge(inputs []io.Reader, out io.Writer) error {
	blocks, err := load(inputs)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("mode: set\n")
	for _, key := range slices.Sorted(maps.Keys(blocks)) {
		count := 0
		if blocks[key].covered {
			count = 1
		}
		fmt.Fprintf(&b, "%s %d %d\n", key, blocks[key].statements, count)
	}
	_, err = io.WriteString(out, b.String())
	return err
}

// checkCovered fails unless every statement in the profiles was covered. A block with
// no statements, such as an empty function body, has nothing to cover. A profile with
// no statements at all did not measure anything, so it does not pass either.
func checkCovered(inputs []io.Reader, out io.Writer) error {
	blocks, err := load(inputs)
	if err != nil {
		return err
	}
	var b strings.Builder
	var total, missed int
	for _, key := range slices.Sorted(maps.Keys(blocks)) {
		total += blocks[key].statements
		if !blocks[key].covered && blocks[key].statements > 0 {
			missed += blocks[key].statements
			fmt.Fprintf(&b, "%s: %d not covered\n", key, blocks[key].statements)
		}
	}
	fmt.Fprintf(&b, "%d of %d statements covered\n", total-missed, total)
	if _, err = io.WriteString(out, b.String()); err != nil {
		return err
	}
	if total == 0 {
		return errors.New("the profile has no statements")
	}
	if missed > 0 {
		return fmt.Errorf("statements not covered: %d", missed)
	}
	return nil
}

func read(in io.Reader, blocks map[string]block) error {
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return errors.New("empty profile")
	}
	// A count from another mode would merge cleanly and mean something else.
	if mode := strings.TrimSpace(scanner.Text()); mode != "mode: set" {
		return fmt.Errorf("unsupported profile header %q", mode)
	}
	for scanner.Scan() {
		key, statements, covered, err := parse(scanner.Text())
		if err != nil {
			return err
		}
		seen, ok := blocks[key]
		if ok && seen.statements != statements {
			return fmt.Errorf("%s has %d statements in one profile and %d in another", key, seen.statements, statements)
		}
		blocks[key] = block{statements: statements, covered: seen.covered || covered}
	}
	return scanner.Err()
}

// parse reads one block, written as "file:start,end statements count".
func parse(line string) (key string, statements int, covered bool, err error) {
	fields := strings.Fields(line)
	if len(fields) == 3 {
		n, statementsErr := strconv.ParseUint(fields[1], 10, 31)
		count, countErr := strconv.ParseUint(fields[2], 10, 64)
		if statementsErr == nil && countErr == nil {
			return fields[0], int(n), count != 0, nil
		}
	}
	return "", 0, false, fmt.Errorf("malformed profile line %q", line)
}
