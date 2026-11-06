// Command covermerge joins Go cover profiles from several operating systems, so
// that _windows.go and _darwin.go files count towards the coverage gate.
//
// A block counts as covered if any profile covered it. Blocks are matched by file
// and position, which are the same on every system built from one commit.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(paths []string, out io.Writer) error {
	if len(paths) == 0 {
		return errors.New("usage: covermerge profile...")
	}
	inputs := make([]io.Reader, 0, len(paths))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		inputs = append(inputs, f)
	}
	return merge(inputs, out)
}

type block struct {
	statements string
	covered    bool
}

func merge(inputs []io.Reader, out io.Writer) error {
	blocks := map[string]block{}
	for _, in := range inputs {
		if err := read(in, blocks); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(blocks))
	for key := range blocks {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("mode: set\n")
	for _, key := range keys {
		count := "0"
		if blocks[key].covered {
			count = "1"
		}
		fmt.Fprintf(&b, "%s %s %s\n", key, blocks[key].statements, count)
	}
	_, err := io.WriteString(out, b.String())
	return err
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
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return fmt.Errorf("malformed profile line %q", scanner.Text())
		}
		key := fields[0]
		seen, ok := blocks[key]
		if ok && seen.statements != fields[1] {
			return fmt.Errorf("%s has %s statements in one profile and %s in another", key, seen.statements, fields[1])
		}
		blocks[key] = block{statements: fields[1], covered: seen.covered || fields[2] != "0"}
	}
	return scanner.Err()
}
